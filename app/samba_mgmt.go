package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Management API for the Samba AD DC node: LDAP users/groups (samba-tool), Kerberos
// (krb5.conf download, per-service principals + keytab export), and TLS cert regeneration.
// All handlers run inside the node's container via execScript and are gated by
// loadRunningNode (auth + running check).

// sambaNode resolves the running Samba node's deployment + parsed config/secrets.
func (a *App) sambaNode(w http.ResponseWriter, r *http.Request) (Deployment, sambaConfig, sambaSecrets, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return Deployment{}, sambaConfig{}, sambaSecrets{}, false
	}
	var cfg sambaConfig
	var sec sambaSecrets
	json.Unmarshal(dep.Config, &cfg)
	json.Unmarshal(dep.Secrets, &sec)
	return dep, cfg, sec, true
}

func (a *App) execLines(containerID, script string, env []string) ([]string, error) {
	out, err := a.execScript(context.Background(), containerID, script, env)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// ------------------------------------------------------------------ LDAP users

// handleSambaUsers lists directory users with attributes, excluding AD built-ins
// (isCriticalSystemObject) and the svc-* Kerberos service accounts (managed on the
// Kerberos tab).
func (a *App) handleSambaUsers(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(context.Background(), dep.ContainerID, sambaUsersScript, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list users: "+err.Error())
		return
	}
	users := []map[string]string{}
	for _, e := range parseLDIF(out) {
		uid := first(e, "sAMAccountName")
		if uid == "" || strings.HasPrefix(uid, "svc-") {
			continue
		}
		users = append(users, map[string]string{
			"uid": uid, "givenName": first(e, "givenName"), "sn": first(e, "sn"),
			"cn": first(e, "displayName"), "mail": first(e, "mail"),
		})
	}
	sort.Slice(users, func(i, j int) bool { return users[i]["uid"] < users[j]["uid"] })
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (a *App) handleSambaUserCreate(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Username, Password, GivenName, Surname, Mail string }
	if err := decode(r, &b); err != nil || !validName(b.Username) || len(b.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "username and a password (≥6 chars) are required")
		return
	}
	env := []string{"U=" + b.Username, "P=" + b.Password, "GN=" + b.GivenName, "SN=" + b.Surname, "MAIL=" + b.Mail}
	if _, err := a.execScript(context.Background(), dep.ContainerID, sambaUserCreateScript, env); err != nil {
		writeErr(w, http.StatusBadGateway, "create user: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSambaUserUpdate replaces a user's attributes (givenName / sn / displayName / mail).
func (a *App) handleSambaUserUpdate(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Username, GivenName, Surname, Cn, Mail string }
	if err := decode(r, &b); err != nil || !validName(b.Username) {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}
	env := []string{"U=" + b.Username, "GN=" + b.GivenName, "SN=" + b.Surname, "CN=" + b.Cn, "MAIL=" + b.Mail}
	if _, err := a.execScript(context.Background(), dep.ContainerID, sambaUserUpdateScript, env); err != nil {
		writeErr(w, http.StatusBadGateway, "update user: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleSambaUserPassword(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Username, Password string }
	if err := decode(r, &b); err != nil || !validName(b.Username) || len(b.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "username and a password (≥6 chars) are required")
		return
	}
	if _, err := a.execScript(context.Background(), dep.ContainerID, `samba-tool user setpassword "$U" --newpassword="$P"`, []string{"U=" + b.Username, "P=" + b.Password}); err != nil {
		writeErr(w, http.StatusBadGateway, "set password: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleSambaUserDelete(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Username string }
	if err := decode(r, &b); err != nil || !validName(b.Username) {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}
	if _, err := a.execScript(context.Background(), dep.ContainerID, `samba-tool user delete "$U"`, []string{"U=" + b.Username}); err != nil {
		writeErr(w, http.StatusBadGateway, "delete user: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ LDAP groups

// dnsDefaultGroups are Samba-provisioned groups that aren't flagged isCriticalSystemObject
// but should still be hidden from the user-facing list.
var dnsDefaultGroups = map[string]bool{"DnsAdmins": true, "DnsUpdateProxy": true}

// handleSambaGroups lists non-built-in groups + their members (usernames). Input is
// "cn: <g>" / "member: <u>" lines from sambaGroupsScript.
func (a *App) handleSambaGroups(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(context.Background(), dep.ContainerID, sambaGroupsScript, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list groups: "+err.Error())
		return
	}
	groups := []map[string]any{}
	var cur map[string]any
	skip := false
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "cn: "); ok {
			cn := strings.TrimSpace(v)
			skip = dnsDefaultGroups[cn]
			if skip {
				cur = nil
				continue
			}
			cur = map[string]any{"cn": cn, "members": []string{}}
			groups = append(groups, cur)
		} else if v, ok := strings.CutPrefix(line, "member: "); ok && cur != nil {
			cur["members"] = append(cur["members"].([]string), strings.TrimSpace(v))
		}
	}
	for _, g := range groups {
		sort.Strings(g["members"].([]string))
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i]["cn"].(string) < groups[j]["cn"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (a *App) handleSambaGroupCreate(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Group string }
	if err := decode(r, &b); err != nil || !validName(b.Group) {
		writeErr(w, http.StatusBadRequest, "group name is required")
		return
	}
	if _, err := a.execScript(context.Background(), dep.ContainerID, `samba-tool group add "$G"`, []string{"G=" + b.Group}); err != nil {
		writeErr(w, http.StatusBadGateway, "create group: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSambaGroupMembers replaces a group's membership with the given comma-separated
// usernames (adds/removes to match), like the Intranet "Set" control.
func (a *App) handleSambaGroupMembers(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Group, Uids string }
	if err := decode(r, &b); err != nil || !validName(b.Group) {
		writeErr(w, http.StatusBadRequest, "group is required")
		return
	}
	for _, u := range strings.Split(b.Uids, ",") {
		if u = strings.TrimSpace(u); u != "" && !validName(u) {
			writeErr(w, http.StatusBadRequest, "invalid username in member list: "+u)
			return
		}
	}
	if _, err := a.execScript(context.Background(), dep.ContainerID, sambaGroupMembersScript, []string{"G=" + b.Group, "UIDS=" + b.Uids}); err != nil {
		writeErr(w, http.StatusBadGateway, "set members: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleSambaGroupDelete(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Group string }
	if err := decode(r, &b); err != nil || !validName(b.Group) {
		writeErr(w, http.StatusBadRequest, "group is required")
		return
	}
	if _, err := a.execScript(context.Background(), dep.ContainerID, `samba-tool group delete "$G"`, []string{"G=" + b.Group}); err != nil {
		writeErr(w, http.StatusBadGateway, "delete group: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ Kerberos

func (a *App) handleSambaKrb5(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	a.serveContainerFile(r.Context(), w, dep.ContainerID, "/etc/krb5.conf", "krb5.conf", "text/plain; charset=utf-8")
}

// handleSambaTargets returns the stack's PostgreSQL + MongoDB node FQDNs to pick a
// principal target from.
func (a *App) handleSambaTargets(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	domain := envOr("DOMAIN", "example.net")
	doc := buildDoc(st)
	hosts := stackHostnames(doc)
	pg, mongo := []string{}, []string{}
	for _, n := range doc.Nodes {
		fqdn := fqdnOf(hosts[n.ID], domain)
		switch n.Type {
		case "pg", "patroni", "repmgr", "spock":
			pg = append(pg, fqdn)
		case "psmdb", "psmrs", "psm":
			mongo = append(mongo, fqdn)
		}
	}
	sort.Strings(pg)
	sort.Strings(mongo)
	writeJSON(w, http.StatusOK, map[string]any{"postgres": pg, "mongodb": mongo})
}

func (a *App) handleSambaPrincipals(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	lines, err := a.execLines(dep.ContainerID, sambaPrincipalsListScript, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list principals: "+err.Error())
		return
	}
	sort.Strings(lines)
	writeJSON(w, http.StatusOK, map[string]any{"principals": lines})
}

var sambaSvcRe = regexp.MustCompile(`^(postgres|mongodb)/[a-zA-Z0-9.-]+$`)

func (a *App) handleSambaPrincipalCreate(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct{ Service, Fqdn string }
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	b.Service = strings.ToLower(strings.TrimSpace(b.Service))
	b.Fqdn = strings.TrimSpace(b.Fqdn)
	principal := b.Service + "/" + b.Fqdn
	if (b.Service != "postgres" && b.Service != "mongodb") || !sambaSvcRe.MatchString(principal) {
		writeErr(w, http.StatusBadRequest, "principal must be postgres/<fqdn> or mongodb/<fqdn>")
		return
	}
	env := []string{"SERVICE=" + b.Service, "FQDN=" + b.Fqdn, "PW=" + genSecret("Svc1!")}
	if _, err := a.execScript(context.Background(), dep.ContainerID, sambaPrincipalCreateScript, env); err != nil {
		writeErr(w, http.StatusBadGateway, "create principal: "+lastLines(err.Error(), 240))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal": principal})
}

func (a *App) handleSambaKeytab(w http.ResponseWriter, r *http.Request) {
	dep, _, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	principal := strings.TrimSpace(r.URL.Query().Get("principal"))
	if !sambaSvcRe.MatchString(principal) {
		writeErr(w, http.StatusBadRequest, "principal must be postgres/<fqdn> or mongodb/<fqdn>")
		return
	}
	if _, err := a.execScript(context.Background(), dep.ContainerID, sambaKeytabScript, []string{"PRINC=" + principal}); err != nil {
		writeErr(w, http.StatusBadGateway, "export keytab: "+lastLines(err.Error(), 200))
		return
	}
	name := strings.ReplaceAll(principal, "/", "_") + ".keytab"
	a.serveContainerFile(r.Context(), w, dep.ContainerID, "/tmp/svc.keytab", name, "application/octet-stream")
}

// ------------------------------------------------------------------ TLS cert

func (a *App) handleSambaCert(w http.ResponseWriter, r *http.Request) {
	dep, cfg, _, ok := a.sambaNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Value int
		Unit  string
	}
	decode(r, &b)
	st, err := a.store.GetStack(dep.StackID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stack not found")
		return
	}
	intranetID := a.intranetContainerID(context.Background(), st)
	if intranetID == "" {
		writeErr(w, http.StatusBadRequest, "an Intranet node (CA) is required to issue a certificate")
		return
	}
	if err := a.sambaApplyCert(context.Background(), dep.ContainerID, intranetID, cfg.FQDN, b.Value, b.Unit, func(string) {}); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.execScript(context.Background(), dep.ContainerID, `systemctl restart samba-ad-dc`, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ scripts

// SAMDB is the local AD database, queried/modified directly (no bind needed as root).
const samdb = "/var/lib/samba/private/sam.ldb"

// sambaUsersScript lists real users (not computers, not AD built-ins) with their attributes.
const sambaUsersScript = `ldbsearch -H ` + samdb + ` "(&(objectClass=user)(!(objectClass=computer))(!(isCriticalSystemObject=TRUE)))" sAMAccountName givenName sn displayName mail 2>/dev/null`

// sambaGroupsScript lists non-built-in groups, each followed by its members' usernames
// (sAMAccountName, via samba-tool listmembers) as "cn: <g>" / "member: <u>" lines.
const sambaGroupsScript = `for g in $(ldbsearch -H ` + samdb + ` "(&(objectClass=group)(!(isCriticalSystemObject=TRUE)))" sAMAccountName 2>/dev/null | sed -n "s/^sAMAccountName: //p"); do
  echo "cn: $g"
  samba-tool group listmembers "$g" 2>/dev/null | while IFS= read -r m; do [ -n "$m" ] && echo "member: $m"; done
done`

const sambaUserCreateScript = `set -e
ARGS=""
[ -n "$GN" ] && ARGS="$ARGS --given-name=$GN"
[ -n "$SN" ] && ARGS="$ARGS --surname=$SN"
[ -n "$MAIL" ] && ARGS="$ARGS --mail-address=$MAIL"
samba-tool user create "$U" "$P" $ARGS`

// sambaUserUpdateScript replaces the given (non-empty) attributes on a user via ldbmodify.
const sambaUserUpdateScript = `set -e
DN=$(ldbsearch -H ` + samdb + ` "(sAMAccountName=$U)" dn --scope=sub 2>/dev/null | sed -n "s/^dn: //p" | head -1)
[ -n "$DN" ] || { echo "user not found"; exit 1; }
LDIF="dn: $DN
changetype: modify"
add() { [ -n "$2" ] && LDIF="$LDIF
replace: $1
$1: $2
-"; }
add givenName "$GN"
add sn "$SN"
add displayName "$CN"
add mail "$MAIL"
[ "$LDIF" = "dn: $DN
changetype: modify" ] && exit 0
printf '%s\n' "$LDIF" | ldbmodify -H ` + samdb + ``

// sambaGroupMembersScript sets a group's membership to exactly $UIDS (comma-separated):
// clear the current members, then add the requested set. Mirrors the Intranet "Set" control.
// Unknown usernames are reported but don't abort the rest.
const sambaGroupMembersScript = `MISSING=""
for u in $(samba-tool group listmembers "$G" 2>/dev/null); do
  [ -n "$u" ] && samba-tool group removemembers "$G" "$u" >/dev/null 2>&1
done
printf '%s\n' "$UIDS" | tr ',' '\n' | while IFS= read -r u; do
  u=$(printf '%s' "$u" | tr -d '[:space:]')
  [ -z "$u" ] && continue
  samba-tool group addmembers "$G" "$u" >/dev/null 2>&1 || echo "could not add $u (no such user)"
done`

const sambaPrincipalCreateScript = `set -e
ACCT="svc-$SERVICE-$(echo "$FQDN" | cut -d. -f1)"
PRINC="$SERVICE/$FQDN"
samba-tool user list | grep -qx "$ACCT" || samba-tool user create "$ACCT" "$PW" --description="service account for $PRINC" >/dev/null
samba-tool user setexpiry "$ACCT" --noexpiry >/dev/null 2>&1 || true
samba-tool spn list "$ACCT" 2>/dev/null | grep -q "$PRINC" || samba-tool spn add "$PRINC" "$ACCT"
echo "$PRINC"`

const sambaPrincipalsListScript = `ldbsearch -H /var/lib/samba/private/sam.ldb "(servicePrincipalName=*)" servicePrincipalName 2>/dev/null | sed -n "s/^servicePrincipalName: \(\(postgres\|mongodb\)\/.*\)$/\1/p" | sort -u`

const sambaKeytabScript = `set -e
rm -f /tmp/svc.keytab
samba-tool domain exportkeytab /tmp/svc.keytab --principal="$PRINC" >/dev/null
test -s /tmp/svc.keytab`
