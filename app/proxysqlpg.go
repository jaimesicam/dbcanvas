package main

import (
	"context"
	"fmt"
	"strings"
)

// proxysqlpg.go — a ProxySQL node in front of PostgreSQL.
//
// ProxySQL 3.0 speaks the PostgreSQL protocol: clients on 6133 (pgsql-interfaces), an admin
// interface on 6132 (admin-pgsql_ifaces) next to the MySQL one on 6032, both listening by
// default, and its own tables — pgsql_servers, pgsql_users, pgsql_replication_hostgroups,
// pgsql_query_rules. Its monitor decides each server's role with pg_is_in_recovery(), so a
// Patroni, repmgr or streaming replication cluster's primary lands in the writer host group and
// the standbys in the reader one, and a switchover or failover moves them. Spock members all
// answer false, so every one is a writer.
//
// Offered on upstream ProxySQL 3.x (proxysqlsource.go) and on a standalone node: the backend is
// what a PgBouncer node can front (pgBouncerBackends — one PostgreSQL node, or a Patroni,
// repmgr, streaming replication or Spock cluster, linked directly). The ProxySQL Cluster frame
// is not offered: its members share configuration through ProxySQL's cluster sync, which
// DBCanvas has not verified for the pgsql_* tables.
//
// Clients and the monitor use the backend's superuser (pgFamilySecrets), as PgBouncer's
// userlist does. Authentication is ProxySQL's default, SCRAM-SHA-256, at both ends, with the
// password held in plain text in pgsql_users.

// ProxySQL's PostgreSQL client and admin ports.
const (
	proxysqlPGPort      = 6133
	proxysqlPGAdminPort = 6132
)

// proxysqlPGBackendKind is the backendKind a PostgreSQL-backed ProxySQL records.
const proxysqlPGBackendKind = "postgresql"

// proxysqlPGBackend is the PostgreSQL backend linked directly to a standalone ProxySQL node.
func proxysqlPGBackend(doc designDoc, nodeID string) (kind string, frame designFrame, node designNode, ok bool) {
	return pgBouncerBackend(doc, nodeID)
}

// proxysqlPGIssues checks a ProxySQL node linked to PostgreSQL: upstream ProxySQL 3.x only.
func proxysqlPGIssues(what string, n designNode) []issue {
	var out []issue
	if !proxysqlIsUpstream(n.ProxySQLSource) {
		out = append(out, issue{Level: "error", Message: what + " fronts PostgreSQL, which DBCanvas configures on upstream ProxySQL — set its source to upstream-native or upstream-admin"})
	}
	if proxysqlMajorOf(n.ProxySQLMajor) != "3" {
		out = append(out, issue{Level: "error", Message: what + " fronts PostgreSQL, which ProxySQL supports from 3.0 — choose the 3 series"})
	}
	return out
}

// proxysqlPGMajor is the PostgreSQL major the psql client is installed for.
func proxysqlPGMajor(kind string, frame designFrame, node designNode) string {
	if kind == "pg" {
		return ppgMajorOf(node.PGMajor)
	}
	return ppgMajorOf(frame.PGMajor)
}

// proxysqlPGLabel names a backend kind for the log.
func proxysqlPGLabel(kind string) string {
	switch kind {
	case "pg":
		return "PostgreSQL"
	case "patroni":
		return "Patroni cluster"
	case "spock":
		return "Spock cluster"
	}
	return pgClusterNoun(kind)
}

// waitProxySQLPGBackend waits for the backend and returns its members (FQDNs), superuser
// credentials and display label — the same waits PgBouncer does for the same backends.
func (a *App) waitProxySQLPGBackend(ctx context.Context, st Stack, doc designDoc, domain, kind string, frame designFrame, node designNode) ([]string, pgSecrets, string, error) {
	sec := pgFamilySecrets()
	switch kind {
	case "pg":
		m, s, err := a.waitPgNodeRunning(ctx, st.ID, node.ID, stackHostnames(doc), domain, deployTimeout())
		return []string{m}, s, node.Label, err
	case "patroni":
		m, s, err := a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
		return m, s, frame.Label, err
	case "repmgr", "pgstream":
		ms, err := a.waitRepmgrMembers(ctx, st.ID, frame, doc, domain, deployTimeout())
		var out []string
		for _, m := range ms {
			out = append(out, m.FQDN)
		}
		return out, sec, frame.Label, err
	case "spock":
		m, s, err := a.waitSpockRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
		return m, s, frame.Label, err
	}
	return nil, sec, "", fmt.Errorf("unsupported PostgreSQL backend %q", kind)
}

// proxysqlPGClientStep installs psql from the backend's ppg-NN repository — the admin interface
// is configured over the PostgreSQL protocol, and psql is what a console user reaches for.
func proxysqlPGClientStep(os, major string) (string, []string) {
	pkgs := pgBouncerPackages(os, major)
	env := []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs[1:], " ")}
	if isDebianOS(os) {
		return pgBouncerInstallDebian, env
	}
	return pgBouncerInstallRHEL, env
}

// proxysqlPGEnv is the environment for proxysqlPGConfigureScript.
func proxysqlPGEnv(sec pgSecrets, members []string, mode string) []string {
	return []string{
		"ADMIN_USER=admin", "ADMIN_PW=" + proxysqlAdminPassword(),
		"APP_USER=" + sec.SuperUser, "APP_PW=" + sec.SuperPassword,
		"MON_USER=" + sec.SuperUser, "MON_PW=" + sec.SuperPassword,
		"SERVERS=" + strings.Join(members, ","),
		"MODE=" + mysqlProxyMode(mode),
	}
}

// proxysqlPGStartScript is proxysqlStartScript over the PostgreSQL admin interface: start
// proxysql, wait for 6132, and move the admin credential off admin/admin. One statement per
// -c: the admin interface takes them one at a time.
const proxysqlPGStartScript = `set -e
systemctl enable --now proxysql >/dev/null 2>&1 || systemctl restart proxysql
P() { PGPASSWORD="$2" psql -h 127.0.0.1 -p 6132 -U "$1" -d admin -Atqw -v ON_ERROR_STOP=1 "${@:3}"; }
CONN=""
for i in $(seq 1 20); do
  if P "$ADMIN_USER" "$ADMIN_PW" -c "SELECT 1" >/dev/null 2>&1; then CONN=new; break; fi
  if P admin admin -c "SELECT 1" >/dev/null 2>&1; then CONN=def; break; fi
  sleep 2
done
[ -n "$CONN" ] || { echo "proxysql PostgreSQL admin interface (6132) did not come up"; exit 1; }
if [ "$CONN" = def ] && [ "$ADMIN_USER:$ADMIN_PW" != "admin:admin" ]; then
  P admin admin -c "UPDATE global_variables SET variable_value='$ADMIN_USER:$ADMIN_PW' WHERE variable_name='admin-admin_credentials'" \
    -c "LOAD ADMIN VARIABLES TO RUNTIME" -c "SAVE ADMIN VARIABLES TO DISK"
fi`

// proxysqlPGConfigureScript wires ProxySQL to a PostgreSQL backend: the monitor credential,
// writer 10 / reader 20 replication host groups (the monitor sorts the members by
// pg_is_in_recovery()), every member in both groups — the monitor takes a standby out of 10, but
// does not add the primary to 20 on its own, which left reads with nowhere to go when there was
// no standby — the client user defaulting to the writer, and, in
// rwsplit, SELECTs to the readers (SELECT … FOR UPDATE/SHARE stays on the writer). primary sends
// everything to the writer. SERVERS is the members' FQDNs.
const proxysqlPGConfigureScript = `set -e
P() { PGPASSWORD="$ADMIN_PW" psql -h 127.0.0.1 -p 6132 -U "$ADMIN_USER" -d admin -Atqw -v ON_ERROR_STOP=1 "$@"; }
P -c "UPDATE global_variables SET variable_value='$MON_USER' WHERE variable_name='pgsql-monitor_username'" \
  -c "UPDATE global_variables SET variable_value='$MON_PW' WHERE variable_name='pgsql-monitor_password'" \
  -c "LOAD PGSQL VARIABLES TO RUNTIME" -c "SAVE PGSQL VARIABLES TO DISK"
args=(-c "DELETE FROM pgsql_replication_hostgroups"
      -c "INSERT INTO pgsql_replication_hostgroups (writer_hostgroup,reader_hostgroup,comment) VALUES (10,20,'dbcanvas')"
      -c "DELETE FROM pgsql_servers")
for h in $(echo "$SERVERS" | tr ',' ' '); do
  args+=(-c "INSERT INTO pgsql_servers (hostgroup_id,hostname,port) VALUES (10,'$h',5432)"
         -c "INSERT INTO pgsql_servers (hostgroup_id,hostname,port) VALUES (20,'$h',5432)")
done
P "${args[@]}" -c "LOAD PGSQL SERVERS TO RUNTIME" -c "SAVE PGSQL SERVERS TO DISK"
P -c "DELETE FROM pgsql_users WHERE username='$APP_USER'" \
  -c "INSERT INTO pgsql_users (username,password,default_hostgroup,active) VALUES ('$APP_USER','$APP_PW',10,1)" \
  -c "LOAD PGSQL USERS TO RUNTIME" -c "SAVE PGSQL USERS TO DISK"
rules=(-c "DELETE FROM pgsql_query_rules")
if [ "$MODE" = rwsplit ]; then
  rules+=(-c "INSERT INTO pgsql_query_rules (rule_id,active,match_digest,destination_hostgroup,apply) VALUES (10,1,'^SELECT.*FOR (UPDATE|SHARE)',10,1)"
          -c "INSERT INTO pgsql_query_rules (rule_id,active,match_digest,destination_hostgroup,apply) VALUES (20,1,'^SELECT',20,1)")
fi
P "${rules[@]}" -c "LOAD PGSQL QUERY RULES TO RUNTIME" -c "SAVE PGSQL QUERY RULES TO DISK"
# The monitor has to have sorted the members before a client is sent anywhere.
for i in $(seq 1 30); do
  [ "$(P -c "SELECT COUNT(*) FROM runtime_pgsql_servers WHERE hostgroup_id=10 AND status='ONLINE'")" -ge 1 ] && exit 0
  sleep 2
done
echo "no PostgreSQL member reached the writer host group (10):"; P -c "SELECT hostgroup_id,hostname,status FROM runtime_pgsql_servers"; exit 1`
