package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// aio_mysql.go — MySQL-family instances inside an All-in-One node.
//
// Kinds: ps (standalone), psrepl (async/semi-sync replication group), innodb
// (InnoDB Cluster / Group Replication) and pxc (Percona XtraDB Cluster).
//
// Everything here exists because the classic path in mysql.go owns its
// container: it initializes /var/lib/mysql, writes /etc/my.cnf, drives the
// vendor `mysqld` unit and connects on 3306. None of that survives having six
// mysqlds in one machine, so this file re-does the same sequence against an
// instLayout — a private datadir, config, socket, unit and port slot per
// instance — while reusing the shared SQL fragments (mysqlAdminUserSQL) and the
// same .env-derived credentials (mysqlFamilySecrets) so accounts are identical
// to every other dbcanvas MySQL node.
//
// Two deliberate differences from mysql.go, both simplifications the classic
// path cannot make:
//
//   - Datadirs are created with `mysqld --initialize-insecure`, so root@localhost
//     starts with an empty password and the temporary-password dance
//     (mysqlSetRootPW) is unnecessary.
//   - Admin SQL always goes over the instance's own unix socket, never TCP.
//     127.0.0.1 and the socket can match *different* accounts, and with N servers
//     on one host a stray TCP connection to the wrong port is a real hazard.
//
// One MySQL flavor per container: percona-server-server and
// percona-xtradb-cluster-server both Provides: mysql-server and cannot both be
// installed. aioMySQLFlavor derives which one a design needs; validateStack
// rejects a design that asks for both.

// aioMySQLPackages is the percona-release product and server package for the PS
// flavor. The PXC flavor installs a different, conflicting package set — see
// aioProvisionMySQL, which rejects it until that path is built.
func aioMySQLPackages(major, os string) (product, pkg string) {
	return psClientProduct(psMajorOf(major)), psServerPackage(os, psMajorOf(major))
}

// aioMySQLMajor is the node-level Percona Server series and pinned minor. One
// install serves every MySQL-family instance in the node, so this is a
// node-level choice rather than a per-instance one.
func aioMySQLMajor(n designNode) (major, version string) {
	return psMajorOf(n.AIOPSMajor), n.AIOPSVersion
}

// aioProvisionMySQL installs the flavor's packages once, then brings up every
// MySQL-family instance the node declares.
func (a *App) aioProvisionMySQL(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg, base, span int) error {
	flavor, conflict := aioMySQLFlavor(n.AIOInstances)
	if conflict {
		// validateStack blocks this before deploy; this is the backstop for a
		// design that reached here another way.
		return fmt.Errorf("this node declares both PXC and Percona Server instances, which cannot share a container")
	}
	major, version := aioMySQLMajor(n)
	if flavor == flavorPXC {
		major, version = aioPXCMajor(n)
		if err := a.aioPXCInstall(ctx, id, n, pr, base); err != nil {
			return err
		}
	} else {
		product, pkg := aioMySQLPackages(major, n.OS)
		pr.phase("Installing Percona Server", base)
		instScript := mysqlInstallRHEL
		if isDebianOS(n.OS) {
			instScript = mysqlInstallDebian
		}
		if err := a.runStep(ctx, id, instScript, []string{"PRODUCT=" + product, "PKG=" + pkg, "VER=" + version}, pr.logln); err != nil {
			return fmt.Errorf("install %s: %w", pkg, err)
		}
		pr.logln(pkg + " installed")

		// Nothing may hold port 3306: the package's own unit would, so mask it
		// before any instance starts. Masking survives a later package upgrade.
		if err := a.runStep(ctx, id, aioMaskVendorUnits, []string{"UNITS=" + mysqlUnit(n.OS) + " mysqld mysql mariadb"}, pr.logln); err != nil {
			return fmt.Errorf("mask vendor mysql units: %w", err)
		}
		pr.logln("vendor mysqld unit masked — instances own their ports")
	}

	mysqls := aioMembersOfFamily(cfg, famMySQL)
	if len(mysqls) == 0 {
		return nil
	}

	if flavor == flavorPXC {
		return a.aioPXCBringUp(ctx, id, n, cfg, fresh, sec, pr, base, span)
	}

	// Phase 1: every NEW member gets its tree, config, datadir, unit — and starts.
	// Existing members are skipped: re-running the baseline below on a live server
	// would RESET its binlog/GTID history and break the replication it is part of.
	for i, m := range mysqls {
		if !fresh[m.Inst] {
			continue
		}
		pr.phase(fmt.Sprintf("Preparing MySQL instance %s (%d/%d)", m.Inst, i+1, len(mysqls)), base+span/4)
		if err := a.aioMySQLPrepare(ctx, id, n, m, major, version, pr); err != nil {
			return err
		}
	}
	// Phase 2: baseline each new server (root password, accounts, GTID reset).
	for _, m := range mysqls {
		if !fresh[m.Inst] {
			continue
		}
		pr.phase("Configuring "+m.Inst, base+span/2)
		if err := a.aioMySQLBaseline(ctx, id, m, major, sec, pr); err != nil {
			return err
		}
	}
	// Let the unix root user run `mysql` against any instance without typing the
	// password — the same affordance mysql.go:363 gives a classic node via
	// /root/.my.cnf. The socket is deliberately omitted (each instance has its
	// own); `aioctl connect` supplies it per instance.
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/root", ".my.cnf", 0o600, aioRootMyCnf(sec)); err != nil {
		pr.logln("write /root/.my.cnf: " + err.Error()) // convenience only, not fatal
	}

	// Phase 3: wire replication within each psrepl group.
	pr.phase("Configuring replication", base+(span*3)/4)
	if err := a.aioMySQLReplicate(ctx, id, n, cfg, sec, major, pr); err != nil {
		return err
	}
	// Phase 4: form each Group Replication group.
	if err := a.aioMySQLFormGroups(ctx, id, n, cfg, sec, pr, base+(span*7)/8); err != nil {
		return err
	}
	return nil
}

// aioGroupUUID is a Group Replication group name: a UUID derived deterministically
// from the instance's own id, so a redeploy re-forms the SAME group rather than
// creating a second one that the existing datadirs would refuse to join.
func aioGroupUUID(instanceID string) string {
	h := sha256.Sum256([]byte("dbcanvas-aio-gr:" + instanceID))
	s := hex.EncodeToString(h[:16])
	// Stamp the RFC-4122 version/variant nibbles — GR validates the UUID shape.
	b := []byte(s)
	b[12] = '4'
	if b[16] < '8' || b[16] > 'b' {
		b[16] = '8'
	}
	s = string(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// aioWriteGRHook stages an instance's bootstrap/join sequence as
// /etc/dbcanvas/aio/<inst>.poststart, which aioctl runs after starting the unit.
// The env the provisioner passed is baked in as exports — the file is 0700
// root-only, the same trust level as /root/.my.cnf.
func (a *App) aioWriteGRHook(ctx context.Context, id string, m aioInstanceRuntime, script string, env []string) error {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("# dbcanvas All-in-One post-start hook — re-forms Group Replication for " + m.Inst + ".\n")
	b.WriteString("# Generated; idempotent (exits early when the member is already ONLINE).\n")
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "export %s=%q\n", k, v)
	}
	// Give mysqld a moment to accept connections before the hook talks to it.
	fmt.Fprintf(&b, "for i in $(seq 1 30); do mysql --socket=%q -uroot -e 'SELECT 1' >/dev/null 2>&1 && break; sleep 1; done\n",
		aioLayout(m.Inst, m.Kind, m.Ports).Sock)
	b.WriteString(script)
	b.WriteString("\n")
	return a.engCtx(ctx).CopyFile(ctx, id, aioEtc, m.Inst+".poststart", 0o700, []byte(b.String()))
}

// aioMySQLGRMembers is one innodb instance's members, bootstrap first.
func aioMySQLGRMembers(cfg aioConfig, group string) []aioInstanceRuntime {
	var out []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Kind == "innodb" && m.Group == group {
			out = append(out, m)
		}
	}
	return out
}

// aioMySQLFormGroups bootstraps each Group Replication group on its first member
// and joins the rest.
//
// Every member is in this one container, so they are distinguished only by port:
// group_replication_local_address is 127.0.0.1:<slot group port> and the seed
// list is every member's. That also breaks the classic readiness check — innodb.go
// matches replication_group_members on MEMBER_HOST=@@hostname, but here every
// member reports the SAME container hostname, so the query would match an
// arbitrary peer. This uses MEMBER_ID=@@server_uuid, which is unique per instance.
func (a *App) aioMySQLFormGroups(ctx context.Context, id string, n designNode, cfg aioConfig, sec pxcSecrets, pr *pxcProg, pct int) error {
	for _, in := range n.AIOInstances {
		if in.Kind != "innodb" {
			continue
		}
		group := aioSanitizeInst(in.Name)
		members := aioMySQLGRMembers(cfg, group)
		if len(members) == 0 {
			continue
		}
		pr.phase("Forming Group Replication "+in.Name, pct)
		for i, m := range members {
			l := aioLayout(m.Inst, m.Kind, m.Ports)
			env := []string{
				"SOCK=" + l.Sock, "ROOT_PW=" + sec.RootPassword,
				"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
			}
			script := aioMySQLGRJoinScript
			if i == 0 {
				script = aioMySQLGRBootstrapScript
			}
			if err := a.runStep(ctx, id, script, env, pr.logln); err != nil {
				return fmt.Errorf("%s: %s group %s: %w",
					m.Inst, map[bool]string{true: "bootstrap", false: "join"}[i == 0], in.Name, err)
			}
			// Stage the same sequence as a post-start hook. Members run with
			// group_replication_start_on_boot=OFF, so after `aioctl start <group>`
			// systemd reports the daemons active while the GROUP is still down —
			// the hook is what makes a restarted group actually re-form.
			if err := a.aioWriteGRHook(ctx, id, m, script, env); err != nil {
				pr.logln(m.Inst + ": stage post-start hook: " + err.Error())
			}
			pr.logln(fmt.Sprintf("%s ONLINE in group %s (127.0.0.1:%d)", m.Inst, in.Name, m.Ports.Group))
		}
	}
	return nil
}

// aioRootMyCnf is /root/.my.cnf for an All-in-One node: credentials only, no
// socket. pxcRootMyCnf pins socket=/var/lib/mysql/mysql.sock because a classic
// node has exactly one server; here the socket is what distinguishes the
// instances, so it must stay the caller's choice.
func aioRootMyCnf(sec pxcSecrets) []byte {
	return []byte("[client]\nuser=" + sec.RootUser + "\npassword=" + sec.RootPassword + "\n")
}

// aioMembersOfFamily filters the node's planned members down to one family,
// preserving plan order (which is also start order).
func aioMembersOfFamily(cfg aioConfig, family string) []aioInstanceRuntime {
	var out []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Family == family {
			out = append(out, m)
		}
	}
	return out
}

// aioMySQLPrepare builds one instance: directories, my.cnf, an initialized
// datadir, a systemd unit, and a started server.
func (a *App) aioMySQLPrepare(ctx context.Context, id string, n designNode, m aioInstanceRuntime, major, version string, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)

	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}
	cnf := aioMySQLCnf(l, m, n, major, version)
	if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "my.cnf", 0o644, []byte(cnf)); err != nil {
		return fmt.Errorf("%s: write my.cnf: %w", m.Inst, err)
	}
	// Initialize the datadir with this instance's own config, so the server's
	// idea of datadir/socket/log matches the unit that will run it.
	if err := a.runStep(ctx, id, aioMySQLInitScript, []string{
		"DATADIR=" + l.DataDir, "LOGERR=" + l.LogErr, "INST=" + m.Inst,
		"SOCK=" + l.Sock, "RUNDIR=" + l.RunDir,
	}, pr.logln); err != nil {
		return fmt.Errorf("%s: initialize datadir: %w", m.Inst, err)
	}

	exec := fmt.Sprintf("/usr/sbin/mysqld --defaults-file=%s", l.ConfPath)
	if m.Kind == "pxc" {
		// Galera needs exactly one member launched with --wsrep-new-cluster; the
		// wrapper decides when (see aioPXCStartWrapper).
		wrapper := l.ConfDir + "/start.sh"
		if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "start.sh", 0o755,
			[]byte(aioPXCStartWrapper(l, m.Role == "bootstrap"))); err != nil {
			return fmt.Errorf("%s: write start wrapper: %w", m.Inst, err)
		}
		exec = wrapper
	}
	spec := aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One %s instance %s (port %d)", m.Kind, m.Inst, m.Ports.Client),
		ExecStart:   exec,
		Type:        "notify",
		TimeoutSec:  600, // an SST joiner can take a while
		EnvFile:     aioMySQLEnvFile(l, m),
	}
	if err := a.aioWriteUnit(ctx, id, l, spec); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s running on port %d (%s)", m.Inst, m.Ports.Client, l.DataDir))
	return nil
}

// aioMySQLCnf renders one instance's my.cnf. Every path and port is the
// instance's own; nothing here may be a product default.
func aioMySQLCnf(l instLayout, m aioInstanceRuntime, n designNode, major, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — instance %s (%s). Generated; edits are lost on redeploy.\n", m.Inst, m.Kind)
	fmt.Fprintf(&b, "[client]\nsocket=%s\nport=%d\n\n", l.Sock, m.Ports.Client)
	b.WriteString("[mysqld]\n")
	fmt.Fprintf(&b, "server-id=%d\n", serverIDFor(m.Inst))
	fmt.Fprintf(&b, "port=%d\nmysqlx_port=%d\n", m.Ports.Client, m.Ports.Admin)
	fmt.Fprintf(&b, "datadir=%s\nsocket=%s\n", l.DataDir, l.Sock)
	fmt.Fprintf(&b, "mysqlx_socket=%s/mysqlx.sock\n", l.RunDir)
	fmt.Fprintf(&b, "log-error=%s\npid-file=%s/mysqld.pid\n", l.LogErr, l.RunDir)
	fmt.Fprintf(&b, "user=mysql\nbind-address=0.0.0.0\n")
	fmt.Fprintf(&b, "slow_query_log=ON\nslow_query_log_file=%s/slow.log\nlong_query_time=2\n", l.LogDir)
	// A container running many servers cannot give each the single-instance
	// default buffer pool; without this a handful of instances exhausts memory.
	b.WriteString("innodb_buffer_pool_size=128M\nperformance_schema=ON\n")
	if aioMySQLGTID(n, m) {
		b.WriteString("gtid_mode=ON\nenforce_gtid_consistency=ON\n")
	}
	fmt.Fprintf(&b, "log_bin=%s/binlog\n%s\nbinlog_format=ROW\n", l.DataDir, logUpdatesOption(major, version))
	fmt.Fprintf(&b, "relay_log=%s/relay-bin\n", l.DataDir)
	switch m.Kind {
	case "innodb":
		b.WriteString(aioMySQLGRSettings(m, n))
	case "pxc":
		b.WriteString(aioPXCSettings(m, n, m.Group, aioPXCMembers(n, m.Group)))
	}
	return b.String()
}

// aioPXCMembers is every member of one PXC cluster instance, in plan order.
func aioPXCMembers(n designNode, group string) []aioInstanceRuntime {
	var out []aioInstanceRuntime
	for _, m := range aioPlan(n, envOr("DOMAIN", "example.net"), "") {
		if m.Kind == "pxc" && m.Group == group {
			out = append(out, m)
		}
	}
	return out
}

// aioMySQLGRSettings is the Group Replication block for an innodb member.
// Everything that would normally be a hostname is 127.0.0.1 plus this member's
// own slot port — inside one container the port is the only distinguishing
// coordinate, for the local address and for every seed.
func aioMySQLGRSettings(m aioInstanceRuntime, n designNode) string {
	var in *aioInstance
	for i := range n.AIOInstances {
		if aioSanitizeInst(n.AIOInstances[i].Name) == m.Group {
			in = &n.AIOInstances[i]
		}
	}
	if in == nil {
		return ""
	}
	// Seeds: every member of this group, including self (GR tolerates that and it
	// keeps the list identical across members, which is easier to reason about).
	total := aioMemberCount(in.Kind, in.Members)
	slots := aioAssignSlots(n.AIOInstances)
	seeds := make([]string, 0, total)
	for i := 0; i < total; i++ {
		p := aioPortsFor(in.Kind, slots[in.ID], i)
		seeds = append(seeds, fmt.Sprintf("127.0.0.1:%d", p.Group))
	}

	var b strings.Builder
	b.WriteString("# --- group replication ---\n")
	b.WriteString("plugin_load_add=group_replication.so\n")
	fmt.Fprintf(&b, "group_replication_group_name=%q\n", aioGroupUUID(in.ID))
	// OFF: the group is bootstrapped/joined explicitly and in order by the
	// provisioner. Starting on boot would race the members against each other.
	b.WriteString("group_replication_start_on_boot=OFF\n")
	fmt.Fprintf(&b, "group_replication_local_address=\"127.0.0.1:%d\"\n", m.Ports.Group)
	fmt.Fprintf(&b, "group_replication_group_seeds=%q\n", strings.Join(seeds, ","))
	b.WriteString("group_replication_single_primary_mode=ON\n")
	b.WriteString("group_replication_enforce_update_everywhere_checks=OFF\n")
	b.WriteString("group_replication_ip_allowlist=\"AUTOMATIC\"\n")
	// The recovery channel authenticates to the donor as the repl user
	// (caching_sha2_password). Without TLS that refuses unless it can fetch the
	// server's public key — see the same note in innodb.go.
	b.WriteString("group_replication_recovery_get_public_key=ON\n")
	return b.String()
}

// aioMySQLGTID reports whether an instance runs with GTID. Replication kinds
// need it for auto-positioning; a standalone follows its own checkbox.
func aioMySQLGTID(n designNode, m aioInstanceRuntime) bool {
	for _, in := range n.AIOInstances {
		if aioSanitizeInst(in.Name) != m.Group && aioSanitizeInst(in.Name) != m.Inst {
			continue
		}
		switch in.Kind {
		case "psrepl", "innodb":
			return true
		default:
			return in.GTID
		}
	}
	return false
}

// aioMySQLEnvFile gives the unit (and anyone reading /etc/dbcanvas/aio/<inst>.env)
// the instance's coordinates, so `aioctl` and ad-hoc shell work agree on them.
func aioMySQLEnvFile(l instLayout, m aioInstanceRuntime) string {
	return fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_SOCKET=%s\nAIO_DATADIR=%s\nAIO_CNF=%s\n",
		m.Inst, m.Kind, m.Ports.Client, l.Sock, l.DataDir, l.ConfPath)
}

// aioMySQLBaseline sets the root password and creates the standard dbcanvas
// accounts on one instance, then clears GTID history so replication starts from
// a shared empty baseline — the same contract mysqlSetupBaseline provides for a
// classic node, over the instance's socket.
func (a *App) aioMySQLBaseline(ctx context.Context, id string, m aioInstanceRuntime, major string, sec pxcSecrets, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	env := []string{
		"SOCK=" + l.Sock,
		"ROOT_PW=" + sec.RootPassword,
		"VPRELAX=" + validatePasswordRelax(major),
		"AUTH_PLUGIN=" + psAuthPlugin(major),
		"MON_GRANTS=" + monitorGrants(major),
		"RESET_CMD=" + mysqlResetCmd(major),
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"ORCH_USER=" + sec.OrchestratorUser, "ORCH_PW=" + sec.OrchestratorPassword,
	}
	if err := a.runStep(ctx, id, aioMySQLBaselineScript, env, pr.logln); err != nil {
		return fmt.Errorf("%s: baseline: %w", m.Inst, err)
	}
	pr.logln(m.Inst + ": root password set; admin/app/repl/monitor accounts created")
	return nil
}

// aioMySQLReplicate attaches each psrepl group's replicas to its primary. Both
// ends are in this container, so the link is 127.0.0.1:<primary port> — the
// clearest demonstration of why nothing may use a default port.
func (a *App) aioMySQLReplicate(ctx context.Context, id string, n designNode, cfg aioConfig, sec pxcSecrets, major string, pr *pxcProg) error {
	for _, in := range n.AIOInstances {
		if in.Kind != "psrepl" {
			continue
		}
		group := aioSanitizeInst(in.Name)
		var primary aioInstanceRuntime
		var replicas []aioInstanceRuntime
		for _, m := range cfg.Instances {
			if m.Group != group {
				continue
			}
			if m.Role == "primary" {
				primary = m
			} else {
				replicas = append(replicas, m)
			}
		}
		if primary.Inst == "" {
			return fmt.Errorf("replication group %s has no primary", in.Name)
		}
		for _, r := range replicas {
			rl := aioLayout(r.Inst, r.Kind, r.Ports)
			env := []string{
				"SOCK=" + rl.Sock,
				"ROOT_PW=" + sec.RootPassword,
				"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
				"SOURCE_HOST=127.0.0.1",
				fmt.Sprintf("SOURCE_PORT=%d", primary.Ports.Client),
			}
			if err := a.runStep(ctx, id, aioMySQLAttachScript, env, pr.logln); err != nil {
				return fmt.Errorf("%s: attach to %s: %w", r.Inst, primary.Inst, err)
			}
			pr.logln(fmt.Sprintf("%s replicating from %s (127.0.0.1:%d)", r.Inst, primary.Inst, primary.Ports.Client))
		}
		if mysqlReplMode(in.ReplMode) == "semisync" {
			if err := a.aioMySQLSemisync(ctx, id, primary, replicas, major, sec, pr); err != nil {
				return err
			}
		}
	}
	return nil
}

// aioMySQLSemisync installs and enables the semi-sync plugins on a group.
func (a *App) aioMySQLSemisync(ctx context.Context, id string, primary aioInstanceRuntime, replicas []aioInstanceRuntime, major string, sec pxcSecrets, pr *pxcProg) error {
	apply := func(m aioInstanceRuntime, plugin, so, enableVar string) error {
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		return a.runStep(ctx, id, aioMySQLSemisyncScript, []string{
			"SOCK=" + l.Sock, "ROOT_PW=" + sec.RootPassword,
			"PLUGIN=" + plugin, "SONAME=" + so, "ENABLEVAR=" + enableVar,
			"SETVAR=" + persistScope(major),
		}, pr.logln)
	}
	p, so, v := semisyncSource(major)
	if err := apply(primary, p, so, v); err != nil {
		return fmt.Errorf("%s: enable semi-sync source: %w", primary.Inst, err)
	}
	p, so, v = semisyncReplica(major)
	for _, r := range replicas {
		if err := apply(r, p, so, v); err != nil {
			return fmt.Errorf("%s: enable semi-sync replica: %w", r.Inst, err)
		}
	}
	pr.logln("semi-sync replication enabled")
	return nil
}

// persistConfigAIO merges discovered runtime facts back into the deployment's
// aioConfig (e.g. the host port Docker actually assigned to an export).
func (a *App) persistConfigAIO(st Stack, nodeID string, mutate func(*aioConfig)) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil {
		return
	}
	var cfg aioConfig
	if json.Unmarshal(dep.Config, &cfg) != nil {
		return
	}
	mutate(&cfg)
	if b, e := json.Marshal(cfg); e == nil {
		dep.Config = b
		a.store.UpsertDeployment(dep)
	}
}

// ------------------------------------------------------------------ scripts

// aioMySQLInitScript initializes one instance's datadir with its own config.
// --initialize-insecure leaves root@localhost with an empty password, which the
// baseline immediately replaces — this avoids parsing a temporary password out
// of a log that N instances are writing to.
// A MINIMAL defaults file is written for the initialize step rather than reusing
// the instance's own config — mirroring mysqlDatadirInit's reasoning in mysql.go.
// The real config carries replication settings (and, for an innodb member, the
// whole group_replication block including plugin_load_add), none of which
// `mysqld --initialize` should see. Only paths matter here.
const aioMySQLInitScript = `set -e
install -d -m 0750 -o mysql -g mysql "$(dirname "$LOGERR")"
install -m 0640 -o mysql -g mysql /dev/null "$LOGERR" 2>/dev/null || true
say_err() { echo "$1:"; grep -iE '\[ERROR\]' "$LOGERR" /tmp/aio-init-$INST.log 2>/dev/null | tail -5; }
if [ ! -f "$DATADIR/mysql.ibd" ] && [ ! -f "$DATADIR/mysql/user.frm" ]; then
  # --initialize refuses a non-empty datadir; clear dotfiles too.
  find "$DATADIR" -mindepth 1 -delete 2>/dev/null || true
  INITCNF=/tmp/aio-init-$INST.cnf
  printf '[mysqld]\nuser=mysql\ndatadir=%s\nsocket=%s\nlog-error=%s\npid-file=%s/mysqld.pid\n' \
    "$DATADIR" "$SOCK" "$LOGERR" "$RUNDIR" > "$INITCNF"
  mysqld --defaults-file="$INITCNF" --initialize-insecure >/tmp/aio-init-$INST.log 2>&1 || { say_err "datadir initialize failed"; exit 1; }
  rm -f "$INITCNF"
  chown -R mysql:mysql "$DATADIR"
fi
exit 0`

// aioStartUnitScript enables and starts an instance unit, then confirms systemd
// really reports it active — a unit that fails immediately would otherwise look
// like a success.
const aioStartUnitScript = `set -e
systemctl daemon-reload
systemctl reset-failed "$UNIT" 2>/dev/null || true
systemctl enable "$UNIT" >/dev/null 2>&1 || true
# restart, not start: the config was just (re)written, and "start" on a unit that
# is somehow already active is a silent no-op — it would leave the OLD config
# running while this script reported success.
systemctl restart "$UNIT" || true
for i in $(seq 1 60); do
  systemctl is-active --quiet "$UNIT" && exit 0
  sleep 1
done
echo "unit $UNIT did not become active:"
systemctl status --no-pager --lines=15 "$UNIT" 2>&1 | tail -20
[ -n "$LOGERR" ] && tail -20 "$LOGERR" 2>/dev/null
exit 1`

// aioMySQLBaselineScript is mysqlBaselineScript's contract (root password, the
// standard accounts, an empty GTID baseline) against one instance's socket. The
// temporary-password branch is gone: --initialize-insecure means root starts
// with no password.
// --no-defaults is load-bearing and non-obvious. Once ANY instance has been
// baselined, /root/.my.cnf exists carrying user+password — so a plain
// `mysql -uroot` against a *newly added* instance (whose root password is still
// empty, straight out of --initialize-insecure) sends that password and gets
// "ERROR 1045 Access denied". That only bites on the second deploy of a node,
// i.e. exactly when an instance is added to a live All-in-One node.
const aioMySQLBaselineScript = `set -e
MB="mysql --no-defaults --socket=$SOCK -uroot"
if $MB -p"$ROOT_PW" -e "SELECT 1" >/dev/null 2>&1; then
  M="$MB -p$ROOT_PW"
elif $MB -e "SELECT 1" >/dev/null 2>&1; then
  # Freshly initialized: root@localhost still has no password.
  $MB -e "$VPRELAX" 2>/dev/null || true
  $MB -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH $AUTH_PLUGIN BY '$ROOT_PW';"
  M="$MB -p$ROOT_PW"
else
  echo "cannot authenticate to $SOCK as root (neither the configured password nor an empty one worked)"
  exit 1
fi
$M -e "$VPRELAX" 2>/dev/null || true
$M <<SQL
SET GLOBAL super_read_only=OFF;
SET GLOBAL read_only=OFF;
` + mysqlAdminUserSQL + `
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW';
GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER '$MON_USER'@'%' IDENTIFIED BY '$MON_PW';
GRANT $MON_GRANTS ON *.* TO '$MON_USER'@'%';
GRANT SELECT ON performance_schema.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
CREATE USER IF NOT EXISTS '$ORCH_USER'@'%' IDENTIFIED BY '$ORCH_PW';
ALTER USER '$ORCH_USER'@'%' IDENTIFIED BY '$ORCH_PW';
GRANT SUPER, PROCESS, REPLICATION SLAVE, REPLICATION CLIENT, RELOAD ON *.* TO '$ORCH_USER'@'%';
FLUSH PRIVILEGES;
SQL
$M -e "$RESET_CMD" 2>/dev/null || true
exit 0`

// aioMySQLAttachScript points a replica at a primary in the SAME container. The
// only structural difference from mysqlAttachScript is that SOURCE_PORT is a
// variable rather than the hardcoded 3306 — in an All-in-One node the port is
// the only thing distinguishing the two servers.
const aioMySQLAttachScript = `set -e
M="mysql --socket=$SOCK -uroot -p$ROOT_PW"
$M -e "STOP REPLICA;" 2>/dev/null || true
$M -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST='$SOURCE_HOST', SOURCE_PORT=$SOURCE_PORT, SOURCE_USER='$REPL_USER', SOURCE_PASSWORD='$REPL_PW', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1;"
$M -e "START REPLICA;"
OK=0
for i in $(seq 1 30); do
  S=$($M -e "SHOW REPLICA STATUS\G" 2>/dev/null)
  if echo "$S" | grep -q "Replica_IO_Running: Yes" && echo "$S" | grep -q "Replica_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
if [ "$OK" != 1 ]; then
  echo "replication did not start:"
  $M -e "SHOW REPLICA STATUS\G" 2>/dev/null | grep -E 'Last_(IO|SQL)_Error|Replica_(IO|SQL)_Running' | head -6
  exit 1
fi
$M -e "SET PERSIST super_read_only=ON;" 2>/dev/null || $M -e "SET GLOBAL super_read_only=ON;"
exit 0`

// aioMySQLSemisyncScript installs + enables one semi-sync plugin on an instance.
const aioMySQLSemisyncScript = `set -e
M="mysql --socket=$SOCK -uroot -p$ROOT_PW"
$M -e "INSTALL PLUGIN $PLUGIN SONAME '$SONAME';" 2>/dev/null || true
$M -e "SET $SETVAR $ENABLEVAR=1;"
exit 0`

// aioMySQLGRPrep is the shared prelude for bootstrap and join.
//
// The already-ONLINE check is FIRST, and that ordering is load-bearing. On a
// redeploy the group is live, which makes every secondary super_read_only — so
// the CREATE USER below would fail with "ERROR 1290 … --super-read-only" before
// the script ever reached a later exit. Anything that writes must sit behind the
// early exit.
//
// sql_log_bin=0 keeps the recovery user local, so the baseline's RESET cannot
// purge it from a joiner that has not synced yet.
const aioMySQLGRPrep = `if [ "$($M -N -e "SELECT MEMBER_STATE FROM performance_schema.replication_group_members WHERE MEMBER_ID=@@server_uuid" 2>/dev/null)" = "ONLINE" ]; then exit 0; fi
$M -e "SET sql_log_bin=0; CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW'; GRANT REPLICATION SLAVE, BACKUP_ADMIN, CONNECTION_ADMIN ON *.* TO '$REPL_USER'@'%'; GRANT GROUP_REPLICATION_STREAM ON *.* TO '$REPL_USER'@'%';" 2>/dev/null || \
$M -e "SET sql_log_bin=0; CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW'; GRANT REPLICATION SLAVE, BACKUP_ADMIN ON *.* TO '$REPL_USER'@'%';"
$M -e "CHANGE REPLICATION SOURCE TO SOURCE_USER='$REPL_USER', SOURCE_PASSWORD='$REPL_PW' FOR CHANNEL 'group_replication_recovery';" 2>/dev/null || true
`

// aioMySQLGRWait is the readiness check shared by bootstrap and join.
//
// It matches on MEMBER_ID=@@server_uuid, NOT MEMBER_HOST=@@hostname as innodb.go
// does. Every instance in an All-in-One node shares the container's hostname, so
// a MEMBER_HOST match would return an arbitrary peer's state — this check could
// pass while the local member was still RECOVERING, or had failed outright.
const aioMySQLGRWait = `
OK=0
for i in $(seq 1 60); do
  S=$($M -N -e "SELECT MEMBER_STATE FROM performance_schema.replication_group_members WHERE MEMBER_ID=@@server_uuid" 2>/dev/null)
  [ "$S" = "ONLINE" ] && { OK=1; break; }
  sleep 2
done
if [ "$OK" != 1 ]; then
  echo "member did not reach ONLINE (state: ${S:-unknown}):"
  $M -e "SELECT MEMBER_ID, MEMBER_PORT, MEMBER_STATE, MEMBER_ROLE FROM performance_schema.replication_group_members" 2>/dev/null | head -10
  exit 1
fi
exit 0`

const aioMySQLGRHead = `set -e
M="mysql --socket=$SOCK -uroot -p$ROOT_PW"
`

// aioMySQLGRBootstrapScript seeds a new group on its first member.
const aioMySQLGRBootstrapScript = aioMySQLGRHead + aioMySQLGRPrep + `
$M -e "SET GLOBAL group_replication_bootstrap_group=ON; START GROUP_REPLICATION; SET GLOBAL group_replication_bootstrap_group=OFF;"
` + aioMySQLGRWait

// aioMySQLGRJoinScript joins a member to an already-bootstrapped group.
const aioMySQLGRJoinScript = aioMySQLGRHead + aioMySQLGRPrep + `
$M -e "START GROUP_REPLICATION;"
` + aioMySQLGRWait

// ---------------------------------------------------------------- PXC

// PXC is the second MySQL flavor. It cannot coexist with Percona Server —
// percona-xtradb-cluster-server and percona-server-server both Provides:
// mysql-server — so a node is one or the other, derived from its instances and
// enforced by validateStack. Several PXC clusters in ONE node are fine; they
// share the single install's version.
//
// Two things differ from the classic path beyond the usual per-instance layout:
//
//   - Galera addressing. Every member is 127.0.0.1, so gcomm, IST and SST all
//     have to be pinned to that member's slot ports; leaving any of them default
//     means the second member silently fails to join (or joins the wrong peer).
//   - Bootstrapping. The classic path starts the package's `mysql@bootstrap`
//     unit; dbcanvas writes its own units here, so ExecStart goes through a
//     generated wrapper that decides whether to pass --wsrep-new-cluster. That
//     also makes restarting a fully-stopped cluster work the way Galera intends,
//     via safe_to_bootstrap in grastate.dat.

// aioPXCMajor is the node-level PXC series and pinned minor.
func aioPXCMajor(n designNode) (major, version string) {
	m := strings.TrimSpace(n.AIOPXCMajor)
	if m != "8.4" {
		m = "8.0"
	}
	return m, n.AIOPXCVersion
}

// aioPXCInstall installs the PXC package set (server + XtraBackup for SST).
func (a *App) aioPXCInstall(ctx context.Context, id string, n designNode, pr *pxcProg, base int) error {
	major, version := aioPXCMajor(n)
	pr.phase("Installing Percona XtraDB Cluster", base)
	proxy := ""
	if n.UseProxy {
		proxy = "http://intranet." + envOr("DOMAIN", "example.net") + ":3128"
	}
	script := pxcInstallRHEL
	if isDebianOS(n.OS) {
		script = pxcInstallDebian
	}
	if err := a.runStep(ctx, id, script, []string{
		"PRODUCT=" + pxcProduct(major), "PKG=percona-xtradb-cluster", "PROXY=" + proxy, "VER=" + version,
	}, pr.logln); err != nil {
		return fmt.Errorf("install percona-xtradb-cluster: %w", err)
	}
	pr.logln("percona-xtradb-cluster installed")

	// SST uses xtrabackup-v2, so every member needs XtraBackup present.
	xbScript := pxcInstallXtrabackupRHEL
	if isDebianOS(n.OS) {
		xbScript = pxcInstallXtrabackupDebian
	}
	if err := a.runStep(ctx, id, xbScript,
		[]string{"PRODUCT=" + pxbProduct(major), "PKG=" + pxbPackage(major)}, pr.logln); err != nil {
		return fmt.Errorf("install %s: %w", pxbPackage(major), err)
	}
	pr.logln(pxbPackage(major) + " installed (SST)")

	// mysql / mysql@bootstrap would claim 3306 and bootstrap their own cluster.
	if err := a.runStep(ctx, id, aioMaskVendorUnits,
		[]string{"UNITS=mysql mysqld mysql@bootstrap mariadb"}, pr.logln); err != nil {
		return fmt.Errorf("mask vendor mysql units: %w", err)
	}
	pr.logln("vendor mysql/mysql@bootstrap units masked — instances own their ports")
	return nil
}

// aioPXCSettings is the Galera block for one member.
//
// Every address is 127.0.0.1 plus a slot port. gmcast.listen_addr and
// ist.recv_addr must be set explicitly: their defaults (:4567 / :4568) are shared
// across the whole host, so the second member in the container would fail to
// bind. wsrep_sst_receive_address likewise — two concurrent SSTs on one host
// otherwise collide on :4444.
func aioPXCSettings(m aioInstanceRuntime, n designNode, cluster string, members []aioInstanceRuntime) string {
	var peers []string
	for _, p := range members {
		peers = append(peers, fmt.Sprintf("127.0.0.1:%d", p.Ports.Group))
	}
	var b strings.Builder
	b.WriteString("# --- galera ---\n")
	fmt.Fprintf(&b, "wsrep_provider=%s\n", galeraProvider(n.OS))
	fmt.Fprintf(&b, "wsrep_cluster_name=%s\n", cluster)
	fmt.Fprintf(&b, "wsrep_cluster_address=gcomm://%s\n", strings.Join(peers, ","))
	fmt.Fprintf(&b, "wsrep_node_name=%s\n", m.Inst)
	fmt.Fprintf(&b, "wsrep_node_address=127.0.0.1:%d\n", m.Ports.Group)
	fmt.Fprintf(&b, "wsrep_provider_options=\"gmcast.listen_addr=tcp://127.0.0.1:%d;ist.recv_addr=127.0.0.1:%d\"\n",
		m.Ports.Group, m.Ports.IST)
	fmt.Fprintf(&b, "wsrep_sst_receive_address=127.0.0.1:%d\n", m.Ports.SST)
	b.WriteString("wsrep_sst_method=xtrabackup-v2\n")
	b.WriteString("binlog_format=ROW\ninnodb_autoinc_lock_mode=2\npxc_strict_mode=ENFORCING\n")
	// Cluster traffic is loopback-only inside one container.
	b.WriteString("pxc_encrypt_cluster_traffic=OFF\n")
	return b.String()
}

// aioPXCStartWrapper is the per-instance ExecStart script.
//
// Galera has no "just start the cluster" command: exactly one member must be
// launched with --wsrep-new-cluster, and picking the wrong moment either splits
// the brain or hangs every member waiting for a primary component. The wrapper
// encodes the two cases an operator would reason about:
//
//   - a fresh datadir on the designated seed (first deploy), and
//   - a clean full shutdown, where Galera itself marks the most advanced member
//     safe_to_bootstrap: 1 in grastate.dat.
//
// Everything else joins normally. Because this lives in ExecStart, `aioctl start
// <group>` brings a stopped cluster back without any extra machinery.
func aioPXCStartWrapper(l instLayout, seed bool) string {
	seedFlag := "0"
	if seed {
		seedFlag = "1"
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
# dbcanvas All-in-One — start wrapper for PXC instance %s. Generated.
CNF=%q
GRASTATE=%q
SEED=%s
EXTRA=""
if [ -f "$GRASTATE" ]; then
  # Galera marks the most advanced member safe_to_bootstrap:1 on a clean shutdown.
  grep -qE '^safe_to_bootstrap:[[:space:]]*1' "$GRASTATE" && EXTRA="--wsrep-new-cluster"
elif [ "$SEED" = 1 ]; then
  # First ever start of the designated seed: nothing to join yet.
  EXTRA="--wsrep-new-cluster"
fi
exec /usr/sbin/mysqld --defaults-file="$CNF" $EXTRA
`, l.Inst, l.ConfPath, l.DataDir+"/grastate.dat", seedFlag)
}

// aioPXCBringUp provisions every PXC cluster in the node: the seed first, then
// each joiner, one at a time.
//
// Order is not cosmetic here. A joiner performs an SST from a live donor, so the
// seed must be up and the cluster PRIMARY before the second member starts;
// starting them together leaves every member waiting for a primary component
// that never forms. The seed is also the only member that gets the standard
// account set — unlike Percona Server replication, Galera replicates DDL/DCL to
// the whole cluster, so creating users on each member would conflict.
func (a *App) aioPXCBringUp(ctx context.Context, id string, n designNode, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg, base, span int) error {
	major, _ := aioPXCMajor(n)
	for _, in := range n.AIOInstances {
		if in.Kind != "pxc" {
			continue
		}
		members := aioPXCMembers(n, aioSanitizeInst(in.Name))
		for i, m := range members {
			if !fresh[m.Inst] {
				continue
			}
			role := "joiner"
			if i == 0 {
				role = "seed"
			}
			pr.phase(fmt.Sprintf("Starting PXC %s member %s (%s, %d/%d)", in.Name, m.Inst, role, i+1, len(members)), base+span/3)
			if err := a.aioMySQLPrepare(ctx, id, n, m, major, "", pr); err != nil {
				return err
			}
			// Wait for this member to join a PRIMARY component before starting the
			// next one, or the joiner has no donor to SST from.
			l := aioLayout(m.Inst, m.Kind, m.Ports)
			if err := a.runStep(ctx, id, aioPXCWaitSyncedScript, []string{
				"SOCK=" + l.Sock, "INST=" + m.Inst,
				"ROOT_PW=" + sec.RootPassword, "LOGERR=" + l.LogErr,
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: did not reach Synced: %w", m.Inst, err)
			}
			if i == 0 {
				pr.phase("Configuring "+m.Inst, base+span/2)
				if err := a.aioMySQLBaseline(ctx, id, m, major, sec, pr); err != nil {
					return err
				}
			}
			pr.logln(fmt.Sprintf("%s Synced in cluster %s (gcomm 127.0.0.1:%d)", m.Inst, in.Name, m.Ports.Group))
		}
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/root", ".my.cnf", 0o600, aioRootMyCnf(sec)); err != nil {
		pr.logln("write /root/.my.cnf: " + err.Error())
	}
	return nil
}

// aioPXCWaitSyncedScript waits until a member reports wsrep_local_state_comment
// = Synced, i.e. it is a full member of a primary component. A member is only a
// usable SST donor once it reaches this.
//
// It has to try BOTH an empty root password and the configured one, because the
// two members differ at this point in the sequence:
//
//   - the seed has a freshly initialized datadir, so root@localhost still has no
//     password (the baseline runs after this wait), while
//   - a joiner's datadir is a byte copy of the donor's, so it already HAS the
//     donor's root password.
//
// Probing only one of them made a perfectly healthy, Synced joiner report
// "state: unknown" until the deploy gave up.
const aioPXCWaitSyncedScript = `set -e
state() {
  mysql --no-defaults --socket="$SOCK" -uroot -N -e "SHOW STATUS LIKE 'wsrep_local_state_comment'" 2>/dev/null | awk '{print $2}'
}
state_pw() {
  mysql --no-defaults --socket="$SOCK" -uroot -p"$ROOT_PW" -N -e "SHOW STATUS LIKE 'wsrep_local_state_comment'" 2>/dev/null | awk '{print $2}'
}
OK=0
for i in $(seq 1 120); do
  S=$(state); [ -z "$S" ] && S=$(state_pw)
  [ "$S" = "Synced" ] && { OK=1; break; }
  sleep 3
done
if [ "$OK" != 1 ]; then
  echo "$INST is not Synced (state: ${S:-unreachable})"
  mysql --no-defaults --socket="$SOCK" -uroot -p"$ROOT_PW" -e "SHOW STATUS LIKE 'wsrep_%'" 2>/dev/null \
    | grep -Ei 'cluster_size|cluster_status|local_state|connected|ready' | head -8
  tail -15 "$LOGERR" 2>/dev/null | cut -c1-160
  exit 1
fi
exit 0`
