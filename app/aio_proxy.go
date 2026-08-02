package main

import (
	"context"
	"fmt"
	"strings"
)

// aio_proxy.go — ProxySQL and HAProxy instances inside an All-in-One node.
//
// These two are where the node type's "drop-downs, not association lines" rule
// actually bites. On the canvas a proxy is wired to its backend by drawing an
// edge to a cluster frame; an All-in-One node has no endpoints at all
// (NODE_TYPES sets ports:false), so a proxy instance names the instance it
// fronts with `backendInstanceId` and the backend's member ports are resolved
// from the plan. validateStack already rejects an empty or incompatible pick, so
// by the time this file runs the reference resolves.
//
// The backends are the sharpest reminder that every port here is allocated
// rather than assumed: a classic HAProxy points at `member:3306`, but here each
// member is `127.0.0.1:<its own slot port>`, and the proxy itself listens on a
// slot port too. Nothing in either config is a product default.
//
// HAProxy comes from the OS repo (no Percona repo needed); ProxySQL comes from
// percona-release, and its admin interface is what dbcanvas configures backends
// through.

// aioProxyBackend resolves a proxy instance's backend: the declaring instance it
// fronts, plus that instance's members in plan order (primary/bootstrap first).
func aioProxyBackend(n designNode, cfg aioConfig, in aioInstance) (aioInstance, []aioInstanceRuntime, bool) {
	b := aioInstanceByID(n.AIOInstances, in.BackendInstance)
	if b == nil {
		return aioInstance{}, nil, false
	}
	key := aioSanitizeInst(b.Name)
	var members []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Group == key || m.Inst == key {
			members = append(members, m)
		}
	}
	return *b, members, len(members) > 0
}

// aioProvisionProxy installs whichever proxies the node needs and brings up each
// instance. Both kinds live here because they solve the same problem and share
// the backend resolution above, even though they come from different repos.
func (a *App) aioProvisionProxy(ctx context.Context, st Stack, n designNode, doc designDoc, id, family string, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg, base, span int) error {
	members := aioMembersOfFamily(cfg, family)
	if len(members) == 0 {
		return nil
	}
	declBy := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if aioFamilyOf(in.Kind) == family {
			declBy[aioSanitizeInst(in.Name)] = in
		}
	}

	switch family {
	case famHAProxy:
		pr.phase("Installing HAProxy", base)
		script := haproxyInstallRHEL
		if isDebianOS(n.OS) {
			script = haproxyInstallDebian
		}
		if err := a.runStep(ctx, id, script, nil, pr.logln); err != nil {
			return fmt.Errorf("install haproxy: %w", err)
		}
		pr.logln("haproxy installed")
		if err := a.runStep(ctx, id, aioMaskVendorUnits, []string{"UNITS=haproxy"}, pr.logln); err != nil {
			return fmt.Errorf("mask vendor haproxy unit: %w", err)
		}
	case famProxy:
		pr.phase("Installing ProxySQL", base)
		major := strings.TrimSpace(n.AIOProxySQLMajor)
		if major == "" {
			major = "2"
		}
		script, clientScript := proxysqlInstallRHEL, proxysqlInstallClientRHEL
		if isDebianOS(n.OS) {
			script, clientScript = proxysqlInstallDebian, proxysqlInstallClientDebian
		}
		if err := a.runStep(ctx, id, script,
			[]string{"PKG=" + proxysqlPackage(major), "VER=" + n.AIOProxySQLVer}, pr.logln); err != nil {
			return fmt.Errorf("install proxysql: %w", err)
		}
		// The admin interface is driven with the mysql client. On a PS-flavored
		// node it is already present; installing it here keeps a proxy-only node
		// working too (the script tolerates an existing install).
		if err := a.runStep(ctx, id, clientScript,
			[]string{"PRODUCT=" + psClientProduct(psMajorOf(n.AIOPSMajor))}, pr.logln); err != nil {
			pr.logln("install percona-server-client: " + err.Error())
		}
		pr.logln("proxysql installed")
		if err := a.runStep(ctx, id, aioMaskVendorUnits, []string{"UNITS=proxysql"}, pr.logln); err != nil {
			return fmt.Errorf("mask vendor proxysql unit: %w", err)
		}
	}
	pr.logln("vendor " + family + " unit masked — instances own their ports")

	for i, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		key := m.Group
		if key == "" {
			key = m.Inst
		}
		in, ok := declBy[key]
		if !ok {
			continue
		}
		backend, backendMembers, ok := aioProxyBackend(n, cfg, in)
		if !ok {
			return fmt.Errorf("%s: backend instance not found (validation should have caught this)", m.Inst)
		}
		pr.phase(fmt.Sprintf("Preparing %s instance %s (%d/%d)", family, m.Inst, i+1, len(members)), base+span/2)
		var err error
		if family == famHAProxy {
			err = a.aioHAProxyPrepare(ctx, id, in, m, backend, backendMembers, pr)
		} else {
			err = a.aioProxySQLPrepare(ctx, id, in, m, backend, backendMembers, sec, pr)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- HAProxy

// aioHAProxyPrepare writes one HAProxy instance's config and unit and starts it.
func (a *App) aioHAProxyPrepare(ctx context.Context, id string, in aioInstance, m aioInstanceRuntime, backend aioInstance, backendMembers []aioInstanceRuntime, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}
	cfgText := aioHAProxyCfg(m, backend, backendMembers)
	if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "haproxy.cfg", 0o644, []byte(cfgText)); err != nil {
		return fmt.Errorf("%s: write haproxy.cfg: %w", m.Inst, err)
	}
	spec := aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One HAProxy instance %s (write %d, read %d)", m.Inst, m.Ports.Client, m.Ports.Check),
		ExecStart:   fmt.Sprintf("/usr/sbin/haproxy -Ws -f %s -p %s/haproxy.pid", l.ConfPath, l.RunDir),
		Type:        "notify", // -Ws is haproxy's master-worker + sd_notify mode
		User:        "root",   // binding is fine as root; haproxy drops privileges itself
		Group:       "root",
		TimeoutSec:  120,
		EnvFile: fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_READ_PORT=%d\nAIO_STATS_PORT=%d\nAIO_CNF=%s\n",
			m.Inst, m.Kind, m.Ports.Client, m.Ports.Check, m.Ports.Admin, l.ConfPath),
	}
	if err := a.aioWriteUnit(ctx, id, l, spec); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioHAProxyCheckScript, []string{"CNF=" + l.ConfPath}, pr.logln); err != nil {
		return fmt.Errorf("%s: config rejected by haproxy -c: %w", m.Inst, err)
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s fronting %s — write :%d, read :%d, stats :%d",
		m.Inst, backend.Name, m.Ports.Client, m.Ports.Check, m.Ports.Admin))
	return nil
}

// aioHAProxyCfg renders one instance's haproxy.cfg.
//
// Health checking differs by backend family, and getting it wrong silently
// routes writes to a replica:
//   - PostgreSQL clusters expose Patroni's REST API, but the AiO PostgreSQL kind
//     is a standalone with no such endpoint, so a plain TCP check is used.
//   - MySQL replication/GR members are checked with a TCP connect too; the
//     primary is listed first and the rest are backups, so writes land on the
//     primary while it is up.
func aioHAProxyCfg(m aioInstanceRuntime, backend aioInstance, members []aioInstanceRuntime) string {
	name := aioSanitizeInst(backend.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — HAProxy instance %s fronting %s. Generated.\n", m.Inst, backend.Name)
	b.WriteString("global\n    maxconn 1000\n    log 127.0.0.1 local0\n\n")
	b.WriteString("defaults\n    log global\n    mode tcp\n    retries 2\n")
	b.WriteString("    timeout client 30m\n    timeout connect 4s\n    timeout server 30m\n    timeout check 5s\n\n")

	fmt.Fprintf(&b, "listen stats\n    mode http\n    bind *:%d\n", m.Ports.Admin)
	b.WriteString("    http-request use-service prometheus-exporter if { path /metrics }\n")
	b.WriteString("    stats enable\n    stats uri /\n    stats refresh 5s\n\n")

	// Writes: the primary first, every other member a backup, so traffic moves on
	// only when the primary is actually down.
	fmt.Fprintf(&b, "listen %s_write\n    bind *:%d\n", name, m.Ports.Client)
	b.WriteString("    option tcp-check\n")
	b.WriteString("    default-server inter 3s fall 3 rise 2 on-marked-down shutdown-sessions\n")
	for i, mem := range members {
		backup := ""
		if i > 0 {
			backup = " backup"
		}
		fmt.Fprintf(&b, "    server %s 127.0.0.1:%d maxconn 1000 check%s\n", mem.Inst, mem.Ports.Client, backup)
	}
	b.WriteString("\n")

	// Reads: balance across everything.
	fmt.Fprintf(&b, "listen %s_read\n    bind *:%d\n", name, m.Ports.Check)
	b.WriteString("    balance roundrobin\n    option tcp-check\n")
	b.WriteString("    default-server inter 3s fall 3 rise 2 on-marked-down shutdown-sessions\n")
	for _, mem := range members {
		fmt.Fprintf(&b, "    server %s 127.0.0.1:%d maxconn 1000 check\n", mem.Inst, mem.Ports.Client)
	}
	return b.String()
}

// ---------------------------------------------------------------- ProxySQL

// aioProxySQLPrepare writes one ProxySQL instance's config and unit, starts it,
// and loads its backends through the admin interface.
func (a *App) aioProxySQLPrepare(ctx context.Context, id string, in aioInstance, m aioInstanceRuntime, backend aioInstance, backendMembers []aioInstanceRuntime, sec pxcSecrets, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "proxysql.cnf", 0o640,
		[]byte(aioProxySQLCnf(l, m, sec))); err != nil {
		return fmt.Errorf("%s: write proxysql.cnf: %w", m.Inst, err)
	}
	if err := a.runStep(ctx, id, aioProxySQLOwnScript, []string{"CNF=" + l.ConfPath, "DATADIR=" + l.DataDir}, pr.logln); err != nil {
		return fmt.Errorf("%s: own proxysql files: %w", m.Inst, err)
	}
	spec := aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One ProxySQL instance %s (mysql %d, admin %d)", m.Inst, m.Ports.Client, m.Ports.Admin),
		// -f with an explicit --datadir: ProxySQL otherwise uses /var/lib/proxysql,
		// which every instance would share (and fight over the sqlite db in).
		ExecStart:  fmt.Sprintf("/usr/bin/proxysql -f -c %s --datadir=%s --idle-threads", l.ConfPath, l.DataDir),
		Type:       "simple",
		User:       "root",
		Group:      "root",
		TimeoutSec: 120,
		EnvFile: fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_ADMIN_PORT=%d\nAIO_DATADIR=%s\nAIO_CNF=%s\n",
			m.Inst, m.Kind, m.Ports.Client, m.Ports.Admin, l.DataDir, l.ConfPath),
	}
	if err := a.aioWriteUnit(ctx, id, l, spec); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}

	// Backends are loaded through the admin interface rather than the config
	// file: ProxySQL persists them in its own sqlite db, and the runtime tables
	// are the authority.
	writer, readers := 10, 20
	var rows []string
	for i, mem := range backendMembers {
		hg := writer
		if i > 0 && mysqlReplMode(in.Mode) != "loadbal" {
			hg = readers
		}
		rows = append(rows, fmt.Sprintf("(%d,'127.0.0.1',%d)", hg, mem.Ports.Client))
	}
	if err := a.runStep(ctx, id, aioProxySQLLoadScript, []string{
		fmt.Sprintf("ADMIN_PORT=%d", m.Ports.Admin),
		"ADMIN_USER=" + sec.ClusterUser, "ADMIN_PW=" + sec.ClusterPassword,
		"SERVERS=" + strings.Join(rows, ","),
		fmt.Sprintf("WRITER=%d", writer), fmt.Sprintf("READER=%d", readers),
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
	}, pr.logln); err != nil {
		return fmt.Errorf("%s: load backends: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s fronting %s (%d backend(s)) — mysql :%d, admin :%d",
		m.Inst, backend.Name, len(backendMembers), m.Ports.Client, m.Ports.Admin))
	return nil
}

// aioProxySQLCnf renders one instance's proxysql.cnf. Only the bootstrap
// settings live here; backends are loaded at runtime through the admin port.
func aioProxySQLCnf(l instLayout, m aioInstanceRuntime, sec pxcSecrets) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — ProxySQL instance %s. Generated.\n", m.Inst)
	fmt.Fprintf(&b, "datadir=%q\n", l.DataDir)
	fmt.Fprintf(&b, "errorlog=%q\n\n", l.LogErr)
	b.WriteString("admin_variables=\n{\n")
	fmt.Fprintf(&b, "    admin_credentials=%q\n", sec.ClusterUser+":"+sec.ClusterPassword)
	fmt.Fprintf(&b, "    mysql_ifaces=%q\n", fmt.Sprintf("0.0.0.0:%d", m.Ports.Admin))
	b.WriteString("}\n\n")
	b.WriteString("mysql_variables=\n{\n")
	b.WriteString("    threads=2\n    max_connections=2048\n")
	fmt.Fprintf(&b, "    interfaces=%q\n", fmt.Sprintf("0.0.0.0:%d", m.Ports.Client))
	fmt.Fprintf(&b, "    monitor_username=%q\n    monitor_password=%q\n", sec.MonitorUser, sec.MonitorPassword)
	b.WriteString("}\n")
	return b.String()
}

// ------------------------------------------------------------------ scripts

// aioHAProxyCheckScript validates a config before the unit ever tries it, so a
// bad backend list surfaces as a clear error rather than a failed start.
const aioHAProxyCheckScript = `set -e
haproxy -c -f "$CNF" >/dev/null`

// aioProxySQLOwnScript hands the config and datadir to root and tightens the
// config (it holds the admin credentials).
const aioProxySQLOwnScript = `set -e
chown root:root "$CNF"; chmod 640 "$CNF"
install -d -m 0750 -o root -g root "$DATADIR"
exit 0`

// aioProxySQLLoadScript loads the backend servers, the monitor/app users and the
// query rules through the admin interface, then persists them to disk.
// Idempotent — every statement is a DELETE+INSERT or a REPLACE.
const aioProxySQLLoadScript = `set -e
A="mysql --no-defaults -h 127.0.0.1 -P $ADMIN_PORT -u $ADMIN_USER -p$ADMIN_PW --protocol=TCP"
for i in $(seq 1 30); do $A -e "SELECT 1" >/dev/null 2>&1 && break; sleep 2; done
$A -e "SELECT 1" >/dev/null || { echo "proxysql admin interface unreachable on $ADMIN_PORT"; exit 1; }
$A <<SQL
DELETE FROM mysql_servers;
INSERT INTO mysql_servers (hostgroup_id, hostname, port) VALUES $SERVERS;
DELETE FROM mysql_replication_hostgroups;
INSERT INTO mysql_replication_hostgroups (writer_hostgroup, reader_hostgroup, comment)
  VALUES ($WRITER, $READER, 'dbcanvas-aio');
DELETE FROM mysql_users;
INSERT INTO mysql_users (username, password, default_hostgroup) VALUES ('$APP_USER','$APP_PW',$WRITER);
UPDATE global_variables SET variable_value='$MON_USER' WHERE variable_name='mysql-monitor_username';
UPDATE global_variables SET variable_value='$MON_PW'   WHERE variable_name='mysql-monitor_password';
LOAD MYSQL SERVERS TO RUNTIME;  SAVE MYSQL SERVERS TO DISK;
LOAD MYSQL USERS TO RUNTIME;    SAVE MYSQL USERS TO DISK;
LOAD MYSQL VARIABLES TO RUNTIME; SAVE MYSQL VARIABLES TO DISK;
SQL
exit 0`
