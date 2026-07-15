package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Intranet node management — all actions run via `docker exec` into the running
// container (no LDAP/SMTP client libraries). Inputs that land in shell scripts
// are passed through the exec environment (never interpolated) and validated.

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validName(s string) bool { return s != "" && len(s) <= 64 && nameRe.MatchString(s) }

func validPassword(s string) bool {
	return len(s) >= 1 && len(s) <= 128 && !strings.ContainsAny(s, ":\n\r")
}

// loadRunningNode resolves the stack + a running node deployment + its secrets.
func (a *App) loadRunningNode(w http.ResponseWriter, r *http.Request) (Deployment, nodeSecrets, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Deployment{}, nodeSecrets{}, false
	}
	dep, err := a.store.GetDeployment(st.ID, r.PathValue("nid"))
	if err != nil || dep.ContainerID == "" {
		writeErr(w, http.StatusNotFound, "node is not deployed")
		return Deployment{}, nodeSecrets{}, false
	}
	if dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return Deployment{}, nodeSecrets{}, false
	}
	dep = a.reconcileContainerID(r.Context(), st.ID, r.PathValue("nid"), dep)
	var sec nodeSecrets
	json.Unmarshal(dep.Secrets, &sec)
	return dep, sec, true
}

// reconcileContainerID re-resolves a node's container by name and persists the stored
// deployment if the id drifted. An out-of-band recreate — e.g. Watchtower upgrading the
// PMM server — keeps the container *name* but assigns a new id, leaving the persisted id
// stale so exec/cert/terminal fail with "No such container" (404). Resolving by name
// (which Watchtower preserves) repairs it transparently.
func (a *App) reconcileContainerID(ctx context.Context, stackID int64, nid string, dep Deployment) Deployment {
	name := containerName(stackID, nid)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok && cid != "" && cid != dep.ContainerID {
		dep.ContainerID = cid
		a.store.UpsertDeployment(Deployment{StackID: stackID, NodeID: nid, ContainerID: cid, State: dep.State, Config: dep.Config, Secrets: dep.Secrets})
	}
	return dep
}

// execScript runs a bash script in the container and returns stdout, mapping a
// non-zero exit to an error with the captured output.
func (a *App) execScript(ctx context.Context, containerID, script string, env []string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", script}, env)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return "", fmt.Errorf("%s", msg)
	}
	return res.Stdout, nil
}

// ldapEnv returns the common LDAP admin environment for a node.
func ldapEnv(sec nodeSecrets) []string {
	return []string{
		"ADMIN_DN=" + sec.LDAPAdminDN,
		"ADMIN_PW=" + sec.LDAPAdminPassword,
		"BASE=" + sec.BaseDN,
	}
}

// parseLDIF parses simple `-LLL` ldapsearch output into a list of attr maps.
func parseLDIF(out string) []map[string][]string {
	var entries []map[string][]string
	cur := map[string][]string{}
	flush := func() {
		if len(cur) > 0 {
			entries = append(entries, cur)
			cur = map[string][]string{}
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, " ") {
			continue // folded continuation — values here are short, skip
		}
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		key := line[:i]
		val := strings.TrimSpace(strings.TrimPrefix(line[i+1:], ":"))
		cur[key] = append(cur[key], val)
	}
	flush()
	return entries
}

func first(m map[string][]string, k string) string {
	if v := m[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// ---------------------------------------------------------------- email users

const emailListScript = `cat /etc/dovecot/users 2>/dev/null | cut -d: -f1`

const emailAddScript = `set -e
EMAIL="$U@$D"
touch /etc/dovecot/users
grep -q "^$EMAIL:" /etc/dovecot/users || echo "$EMAIL:{PLAIN}$P::::::" >> /etc/dovecot/users
touch /etc/postfix/vmailbox
grep -q "^$EMAIL " /etc/postfix/vmailbox || echo "$EMAIL $D/$U/" >> /etc/postfix/vmailbox
postmap /etc/postfix/vmailbox
install -d -o vmail -g vmail "/var/mail/vhosts/$D/$U" 2>/dev/null || true
systemctl reload postfix dovecot 2>/dev/null || systemctl restart postfix dovecot 2>/dev/null || true`

const emailPasswordScript = `set -e
EMAIL="$U@$D"
grep -q "^$EMAIL:" /etc/dovecot/users || { echo "no such user"; exit 1; }
sed -i "s|^$EMAIL:.*|$EMAIL:{PLAIN}$P::::::|" /etc/dovecot/users
systemctl reload dovecot 2>/dev/null || true`

const emailDeleteScript = `set -e
EMAIL="$U@$D"
sed -i "/^$EMAIL:/d" /etc/dovecot/users 2>/dev/null || true
sed -i "/^$EMAIL /d" /etc/postfix/vmailbox 2>/dev/null || true
postmap /etc/postfix/vmailbox 2>/dev/null || true
systemctl reload postfix dovecot 2>/dev/null || true`

// localPart strips a domain from a possibly-qualified address.
func localPart(s string) string {
	if i := strings.Index(s, "@"); i >= 0 {
		return s[:i]
	}
	return s
}

func (a *App) handleEmailList(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, emailListScript, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	users := []string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			users = append(users, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

type emailBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *App) emailMutate(script string, needPw bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, sec, ok := a.loadRunningNode(w, r)
		if !ok {
			return
		}
		var b emailBody
		if err := decode(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		u := localPart(strings.TrimSpace(b.Username))
		if !validName(u) {
			writeErr(w, http.StatusBadRequest, "invalid username")
			return
		}
		env := []string{"U=" + u, "D=" + sec.Domain}
		if needPw {
			if !validPassword(b.Password) {
				writeErr(w, http.StatusBadRequest, "invalid password (no ':' or newlines)")
				return
			}
			env = append(env, "P="+b.Password)
		}
		if _, err := a.execScript(r.Context(), dep.ContainerID, script, env); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// ---------------------------------------------------------------- ldap users

const ldapUsersScript = `ldapsearch -x -LLL -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" -b "ou=People,$BASE" "(objectClass=inetOrgPerson)" uid givenName sn cn mail`

const ldapUserCreateScript = `set -e
LDIF="dn: uid=$U,ou=People,$BASE
objectClass: inetOrgPerson
uid: $U
cn: ${CN:-$U}
sn: ${SN:-$U}"
[ -n "$GN" ] && LDIF="$LDIF
givenName: $GN"
[ -n "$MAIL" ] && LDIF="$LDIF
mail: $MAIL"
echo "$LDIF" | ldapadd -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW"
[ -n "$P" ] && ldappasswd -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" -s "$P" "uid=$U,ou=People,$BASE"
true`

const ldapUserUpdateScript = `set -e
BODY=""
N=0
add() {
  if [ "$N" -gt 0 ]; then BODY="$BODY
-"; fi
  BODY="$BODY
replace: $1
$1: $2"
  N=$((N+1))
}
[ -n "$CN" ] && add cn "$CN"
[ -n "$SN" ] && add sn "$SN"
[ -n "$GN" ] && add givenName "$GN"
[ -n "$MAIL" ] && add mail "$MAIL"
[ "$N" -eq 0 ] && { echo "no changes"; exit 0; }
printf "dn: uid=%s,ou=People,%s\nchangetype: modify%s\n" "$U" "$BASE" "$BODY" | \
  ldapmodify -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW"`

const ldapUserPasswordScript = `ldappasswd -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" -s "$P" "uid=$U,ou=People,$BASE"`

const ldapUserDeleteScript = `ldapdelete -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" "uid=$U,ou=People,$BASE"`

func (a *App) handleLdapUsers(w http.ResponseWriter, r *http.Request) {
	dep, sec, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, ldapUsersScript, ldapEnv(sec))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	users := []map[string]string{}
	for _, e := range parseLDIF(out) {
		users = append(users, map[string]string{
			"uid": first(e, "uid"), "givenName": first(e, "givenName"),
			"sn": first(e, "sn"), "cn": first(e, "cn"), "mail": first(e, "mail"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

type ldapUserBody struct {
	UID       string `json:"uid"`
	Password  string `json:"password"`
	GivenName string `json:"givenName"`
	SN        string `json:"sn"`
	CN        string `json:"cn"`
	Mail      string `json:"mail"`
}

func (a *App) ldapUserEnv(sec nodeSecrets, b ldapUserBody) []string {
	return append(ldapEnv(sec),
		"U="+b.UID, "P="+b.Password, "GN="+b.GivenName, "SN="+b.SN, "CN="+b.CN, "MAIL="+b.Mail)
}

func (a *App) ldapUserMutate(script string, requirePw bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, sec, ok := a.loadRunningNode(w, r)
		if !ok {
			return
		}
		var b ldapUserBody
		if err := decode(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		b.UID = strings.TrimSpace(b.UID)
		if !validName(b.UID) {
			writeErr(w, http.StatusBadRequest, "invalid uid")
			return
		}
		if requirePw && !validPassword(b.Password) {
			writeErr(w, http.StatusBadRequest, "invalid password")
			return
		}
		if b.Password != "" && !validPassword(b.Password) {
			writeErr(w, http.StatusBadRequest, "invalid password")
			return
		}
		if _, err := a.execScript(r.Context(), dep.ContainerID, script, a.ldapUserEnv(sec, b)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// ---------------------------------------------------------------- ldap groups

const ldapGroupsScript = `ldapsearch -x -LLL -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" -b "ou=Groups,$BASE" "(objectClass=posixGroup)" cn gidNumber memberUid`

const ldapGroupCreateScript = `set -e
MAXGID=$(ldapsearch -x -LLL -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" -b "ou=Groups,$BASE" "(objectClass=posixGroup)" gidNumber 2>/dev/null | awk '/^gidNumber:/{print $2}' | sort -n | tail -1)
GID=$(( ${MAXGID:-9999} + 1 ))
echo "dn: cn=$CN,ou=Groups,$BASE
objectClass: posixGroup
cn: $CN
gidNumber: $GID" | ldapadd -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW"`

const ldapGroupMembersScript = `set -e
MODS="dn: cn=$CN,ou=Groups,$BASE
changetype: modify
replace: memberUid"
IFS=',' read -ra ARR <<< "$UIDS"
for u in "${ARR[@]}"; do
  u=$(echo "$u" | tr -d '[:space:]')
  [ -n "$u" ] && MODS="$MODS
memberUid: $u"
done
echo "$MODS" | ldapmodify -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW"`

const ldapGroupDeleteScript = `ldapdelete -x -H ldap://localhost -D "$ADMIN_DN" -w "$ADMIN_PW" "cn=$CN,ou=Groups,$BASE"`

func (a *App) handleLdapGroups(w http.ResponseWriter, r *http.Request) {
	dep, sec, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, ldapGroupsScript, ldapEnv(sec))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups := []map[string]any{}
	for _, e := range parseLDIF(out) {
		groups = append(groups, map[string]any{
			"cn": first(e, "cn"), "gidNumber": first(e, "gidNumber"), "members": e["memberUid"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

type ldapGroupBody struct {
	CN   string `json:"cn"`
	UIDs string `json:"uids"`
}

func (a *App) ldapGroupMutate(script string, withUIDs bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dep, sec, ok := a.loadRunningNode(w, r)
		if !ok {
			return
		}
		var b ldapGroupBody
		if err := decode(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		b.CN = strings.TrimSpace(b.CN)
		if !validName(b.CN) {
			writeErr(w, http.StatusBadRequest, "invalid group name")
			return
		}
		env := append(ldapEnv(sec), "CN="+b.CN)
		if withUIDs {
			// validate each uid
			for _, u := range strings.Split(b.UIDs, ",") {
				if u = strings.TrimSpace(u); u != "" && !validName(u) {
					writeErr(w, http.StatusBadRequest, "invalid uid in member list: "+u)
					return
				}
			}
			env = append(env, "UIDS="+b.UIDs)
		}
		if _, err := a.execScript(r.Context(), dep.ContainerID, script, env); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// ---------------------------------------------------------------- certificate

const certInfoScript = `if [ -f /etc/pki/dbcanvas/intranet.crt ]; then
  echo "exists: yes"
  openssl x509 -in /etc/pki/dbcanvas/intranet.crt -noout -subject -startdate -enddate
  echo -n "eku: "; openssl x509 -in /etc/pki/dbcanvas/intranet.crt -noout -ext extendedKeyUsage 2>/dev/null | tail -1 | sed 's/^ *//'
else
  echo "exists: no"
fi`

const certGenerateScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
DIR=/etc/pki/dbcanvas
install -d "$DIR"
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
if [ -f "$DIR/intranet.crt" ]; then
  TS=$(date -u +%Y%m%d%H%M%S)
  install -d "$DIR/archive"
  mv "$DIR/intranet.crt" "$DIR/archive/intranet-$TS.crt"
  [ -f "$DIR/intranet.key" ] && mv "$DIR/intranet.key" "$DIR/archive/intranet-$TS.key"
fi
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/intranet.key" -out /tmp/intranet.csr -subj "/O=DBCanvas/CN=intranet.$DOMAIN" >/dev/null 2>&1
cat >/tmp/intranet.ext <<EXT
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=DNS:intranet,DNS:intranet.$DOMAIN
EXT
openssl x509 -req -in /tmp/intranet.csr -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" -CAcreateserial \
  -out "$DIR/intranet.crt" -extfile /tmp/intranet.ext -not_after "$END" >/dev/null 2>&1
chmod 600 "$DIR/intranet.key"
rm -f /tmp/intranet.csr /tmp/intranet.ext
openssl x509 -in "$DIR/intranet.crt" -noout -enddate`

func (a *App) handleCertInfo(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, certInfoScript, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": strings.TrimSpace(out)})
}

func (a *App) handleCertGenerate(w http.ResponseWriter, r *http.Request) {
	dep, sec, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Value int    `json:"value"`
		Unit  string `json:"unit"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if b.Value <= 0 {
		b.Value = 365
		b.Unit = "days"
	}
	switch b.Unit {
	case "minutes", "hours", "days":
	default:
		b.Unit = "days"
	}
	env := []string{
		fmt.Sprintf("VALUE=%d", b.Value),
		"UNIT=" + b.Unit,
		"DOMAIN=" + sec.Domain,
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, certGenerateScript, env)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "result": strings.TrimSpace(out)})
}
