package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// aio_pg.go — PostgreSQL instances inside an All-in-One node.
//
// Kind: pg (standalone). The clustered PostgreSQL kinds — patroni, repmgr,
// spock — are not built yet; validateStack rejects them, so a design cannot
// silently deploy without them.
//
// PostgreSQL is the *only* family with no shared-version constraint. Percona's
// PPG packages are per-major and install side by side (`percona-postgresql16-server`
// → /usr/pgsql-16/bin, `…17-server` → /usr/pgsql-17/bin), so two instances in one
// container can genuinely run different majors. Every other family installs one
// unversioned package set and therefore has a single node-level version. That is
// why aioInstance carries PGMajor per instance while PS/PSMDB/Valkey majors live
// on the node.
//
// The classic path (pg.go) drives the packaged `postgresql-NN` unit against the
// package's own /var/lib/pgsql/NN/data. Here each instance gets its own PGDATA
// under /opt/aio/<inst>/data, its own port, and a dbcanvas-authored unit, with
// the packaged unit masked so it cannot claim 5432.

// aioPGMajor is an instance's PostgreSQL major, defaulted the same way the
// classic node does.
func aioPGMajor(in aioInstance) string { return ppgMajorOf(in.PGMajor) }

// aioProvisionPG installs each distinct PostgreSQL major the node needs, then
// brings up every pg instance.
func (a *App) aioProvisionPG(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, pr *pxcProg, base, span int) error {
	if isDebianOS(n.OS) {
		// Debian's PPG packaging is cluster-managed (pg_createcluster +
		// postgresql@NN-main), a different model from the explicit initdb this
		// file uses. Rather than half-support it, say so.
		return fmt.Errorf("All-in-One PostgreSQL instances are only available on Oracle Linux for now")
	}

	// Group the instances by major so each package set installs exactly once,
	// even when the node mixes majors.
	byMajor := map[string][]aioInstance{}
	for _, in := range n.AIOInstances {
		if aioFamilyOf(in.Kind) == famPG {
			m := aioPGMajor(in)
			byMajor[m] = append(byMajor[m], in)
		}
	}
	majors := make([]string, 0, len(byMajor))
	for m := range byMajor {
		majors = append(majors, m)
	}
	sort.Strings(majors)

	// Which distribution each major needs. Percona and PGDG cannot share a major
	// (see aioPGFlavorConflicts) but different majors are independent, so this is
	// resolved per major rather than per node.
	flavorOf := map[string]string{}
	for _, in := range n.AIOInstances {
		if f := aioPGFlavorOfKind(in.Kind); f != "" {
			flavorOf[aioPGMajor(in)] = f
		}
	}

	for i, major := range majors {
		pct := base + (i*span)/(len(majors)*4+1)
		if flavorOf[major] == pgFlavorSource {
			// Spock compiles its own PostgreSQL into /usr/pgsql-NN; there is
			// nothing to install from a repo here (see aioProvisionSpock).
			continue
		}
		if flavorOf[major] == pgFlavorPGDG {
			// repmgr/Spock are built against PGDG, not Percona — repmgr_NN
			// requires postgresqlNN-server, which percona-postgresqlNN-server does
			// not provide (verified by repoquery; see aio_pg.go's flavor section).
			pr.phase(fmt.Sprintf("Installing PostgreSQL %s + repmgr (PGDG)", major), pct)
			if err := a.runStep(ctx, id, repmgrInstallRHEL,
				[]string{"PKGS=" + aioRepmgrPackages(major), "VER="}, pr.logln); err != nil {
				return fmt.Errorf("install PGDG PostgreSQL %s + repmgr: %w", major, err)
			}
			pr.logln("PostgreSQL " + major + " + repmgr installed (PGDG)")
		} else {
			pr.phase(fmt.Sprintf("Installing PostgreSQL %s", major), pct)
			pkgs := pgServerPackages(n.OS, major)
			for _, in := range byMajor[major] {
				if in.Kind == "patroni" {
					// Each member co-locates an etcd; Patroni itself manages postgres.
					pkgs = append(pkgs, "percona-patroni", "etcd")
					break
				}
			}
			env := []string{
				"PRODUCT=" + ppgProduct(major),
				"PKGS=" + strings.Join(pkgs, " "),
				"VER=" + byMajor[major][0].PGVersion,
			}
			if err := a.runStep(ctx, id, patroniInstallRHEL, env, pr.logln); err != nil {
				return fmt.Errorf("install PostgreSQL %s: %w", major, err)
			}
			pr.logln("PostgreSQL " + major + " installed")
		}
		// The packaged postgresql-NN unit would initdb into the shared
		// /var/lib/pgsql/NN/data and bind 5432. Mask it.
		if err := a.runStep(ctx, id, aioMaskVendorUnits,
			[]string{"UNITS=" + pgServiceName(n.OS, major) + " postgresql postgresql-" + major}, pr.logln); err != nil {
			return fmt.Errorf("mask vendor postgresql unit: %w", err)
		}
	}
	pr.logln("vendor postgresql unit(s) masked — instances own their ports")

	members := aioMembersOfFamily(cfg, famPG)
	byInst := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if aioFamilyOf(in.Kind) == famPG {
			byInst[aioSanitizeInst(in.Name)] = in
		}
	}
	for i, m := range members {
		if m.Kind != "pg" {
			continue // clustered kinds are brought up as a unit, below
		}
		in, ok := byInst[m.Inst]
		if !ok {
			continue
		}
		if !fresh[m.Inst] {
			continue // already provisioned; initdb/config would be a no-op at best
		}
		pr.phase(fmt.Sprintf("Preparing PostgreSQL instance %s (%d/%d)", m.Inst, i+1, len(members)), base+span/2)
		if err := a.aioPGPrepare(ctx, id, n, in, m, pr); err != nil {
			return err
		}
	}

	// Clustered kinds are brought up as ordered units: a repmgr standby cannot
	// clone before its primary exists, and a Patroni agent cannot elect a leader
	// before its etcd quorum does.
	for _, in := range n.AIOInstances {
		switch in.Kind {
		case "repmgr":
			if err := a.aioProvisionRepmgr(ctx, id, n, in, cfg, fresh, pr, base, span); err != nil {
				return err
			}
		case "patroni":
			if err := a.aioProvisionPatroni(ctx, id, n, in, cfg, fresh, pr, base, span); err != nil {
				return err
			}
		case "spock":
			if err := a.aioProvisionSpock(ctx, id, n, in, cfg, fresh, pr, base, span); err != nil {
				return err
			}
		}
	}
	return nil
}

// aioPGPrepare initialises one instance's PGDATA, writes its config and unit,
// starts it, and sets the superuser password.
func (a *App) aioPGPrepare(ctx context.Context, id string, n designNode, in aioInstance, m aioInstanceRuntime, pr *pxcProg) error {
	major := aioPGMajor(in)
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	bin := pgBinDir(n.OS, major)

	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}
	// initdb refuses a group/world-readable PGDATA, so it needs 0700 rather than
	// the 0750 the shared mkdir helper uses.
	if err := a.runStep(ctx, id, aioPGInitScript, []string{
		"BINDIR=" + bin, "DATADIR=" + l.DataDir, "LOGDIR=" + l.LogDir,
	}, pr.logln); err != nil {
		return fmt.Errorf("%s: initdb: %w", m.Inst, err)
	}

	// Append dbcanvas settings last so they win over the packaged defaults, and
	// open host auth — the classic node does the same, but the port here is the
	// instance's own rather than 5432.
	if err := a.runStep(ctx, id, aioPGConfigureScript, []string{
		"DATADIR=" + l.DataDir,
		"LOGERR=" + l.LogErr,
		"RUNDIR=" + l.RunDir,
		fmt.Sprintf("PORT=%d", m.Ports.Client),
	}, pr.logln); err != nil {
		return fmt.Errorf("%s: configure: %w", m.Inst, err)
	}

	if err := a.aioPGWriteUnit(ctx, id, l, m, bin, major); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}

	pw := strings.TrimSpace(in.RootPassword)
	if pw == "" {
		pw = envOr("POSTGRES_PASSWORD", "postgres_password")
	}
	if err := a.runStep(ctx, id, aioPGPasswordScript, []string{
		"BINDIR=" + bin, "RUNDIR=" + l.RunDir,
		fmt.Sprintf("PORT=%d", m.Ports.Client), "PGPW=" + pw,
	}, pr.logln); err != nil {
		return fmt.Errorf("%s: set superuser password: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s running on port %d (PostgreSQL %s, %s)", m.Inst, m.Ports.Client, major, l.DataDir))
	return nil
}

// aioPGWriteUnit writes one PostgreSQL instance's systemd unit. Shared by the pg
// and repmgr kinds — they differ in how the datadir is created, not in how the
// server is run.
func (a *App) aioPGWriteUnit(ctx context.Context, id string, l instLayout, m aioInstanceRuntime, bin, label string) error {
	// Packaged PostgreSQL is built --with-systemd and reports readiness via
	// sd_notify, so Type=notify is right and means "accepting connections".
	// Spock's PostgreSQL is compiled from source WITHOUT that flag (see
	// spockCompileScript's configure line), so it never sends READY and a
	// Type=notify unit sits in "activating" forever — even though the server is up
	// and logging "database system is ready to accept connections". Observed live.
	// Type=simple is correct there; every caller already gates on pg_isready
	// before using the instance, so nothing depends on the unit meaning "ready".
	unitType := "notify"
	if m.Kind == "spock" {
		unitType = "simple"
	}
	return a.aioWriteUnit(ctx, id, l, aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One PostgreSQL %s instance %s (port %d)", label, m.Inst, m.Ports.Client),
		// -D is passed explicitly: without it postgres falls back to PGDATA or the
		// compiled-in default, which is the shared packaged datadir.
		ExecStart:  fmt.Sprintf("%s/postgres -D %s -k %s", bin, l.DataDir, l.RunDir),
		Type:       unitType,
		User:       "postgres",
		Group:      "postgres",
		TimeoutSec: 300,
		EnvFile:    aioPGEnvFile(l, m, bin),
	})
}

// aioPGEnvFile records the instance's coordinates for aioctl and ad-hoc shell use.
func aioPGEnvFile(l instLayout, m aioInstanceRuntime, bin string) string {
	return fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_SOCKET=%s\nAIO_DATADIR=%s\nPGDATA=%s\nPGPORT=%d\nPGBIN=%s\n",
		m.Inst, m.Kind, m.Ports.Client, l.RunDir, l.DataDir, l.DataDir, m.Ports.Client, bin)
}

// ------------------------------------------------------------------ scripts

// aioPGInitScript initialises one instance's PGDATA. Guarded on PG_VERSION so a
// redeploy against an existing datadir is a no-op, matching pgInitScript.
const aioPGInitScript = `set -e
install -d -m 0700 -o postgres -g postgres "$DATADIR"
install -d -m 0750 -o postgres -g postgres "$LOGDIR"
if [ ! -s "$DATADIR/PG_VERSION" ]; then
  runuser -u postgres -- "$BINDIR/initdb" -D "$DATADIR" -E UTF8 -k \
    --auth-local=peer --auth-host=scram-sha-256 >/dev/null
fi
exit 0`

// aioPGConfigureScript pins the instance's port, socket directory and log file,
// and opens scram-authenticated host access. Appended last so these win over the
// values initdb wrote.
const aioPGConfigureScript = `set -e
CONF="$DATADIR/postgresql.conf"
HBA="$DATADIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
# Idempotent: strip any previous dbcanvas block before appending a fresh one, so
# a redeploy does not stack duplicate (and possibly stale) settings.
sed -i '/^# --- dbcanvas ---$/,$d' "$CONF"
{
  echo "# --- dbcanvas ---"
  echo "listen_addresses = '*'"
  echo "port = $PORT"
  echo "unix_socket_directories = '$RUNDIR'"
  echo "password_encryption = scram-sha-256"
  echo "logging_collector = off"
  echo "log_min_duration_statement = 2000"
} >> "$CONF"
grep -q "dbcanvas-aio" "$HBA" || {
  echo "# dbcanvas-aio" >> "$HBA"
  echo "host all all 0.0.0.0/0 scram-sha-256" >> "$HBA"
  echo "host all all ::/0      scram-sha-256" >> "$HBA"
}
chown postgres:postgres "$CONF" "$HBA"
exit 0`

// aioPGPasswordScript sets the postgres superuser password over the instance's
// own socket (peer auth as the postgres unix user).
const aioPGPasswordScript = `set -e
for i in $(seq 1 30); do
  runuser -u postgres -- "$BINDIR/pg_isready" -h "$RUNDIR" -p "$PORT" >/dev/null 2>&1 && break
  sleep 1
done
runuser -u postgres -- "$BINDIR/psql" -h "$RUNDIR" -p "$PORT" -v ON_ERROR_STOP=1 \
  -c "ALTER USER postgres WITH PASSWORD '$PGPW';" >/dev/null
exit 0`

// ---------------------------------------------------------------- PG flavor

// PostgreSQL has its own packaging conflict, and it is NOT the same shape as
// MySQL's — this was verified by repoquery against a live container rather than
// assumed:
//
//	dnf repoquery --requires repmgr_16   → postgresql16-server
//	dnf repoquery --provides percona-postgresql16-server → (nothing matching)
//
// So repmgr (and Spock, which is likewise built against PGDG) cannot be
// installed on top of Percona's PPG PostgreSQL. But unlike MySQL, where the
// conflict is node-wide, PostgreSQL packages are per-major: percona-postgresql16
// and postgresql17 co-install happily because they are different packages in
// different prefixes. The rule is therefore **per major**, not per node — a node
// may run a Percona `pg` on 16 and a PGDG `repmgr` on 17, just not both on 16.
// Three distributions, not two. Spock is the third and least obvious: it does
// not install packages at all — it compiles PostgreSQL from source with pgEdge's
// patches and installs the result to /usr/pgsql-NN, the SAME prefix the Percona
// and PGDG packages own. So a Spock instance conflicts with both, for a different
// reason (a file-level collision rather than an RPM dependency).
const (
	pgFlavorPercona = "percona" // pg, patroni → percona-postgresqlNN-*
	pgFlavorPGDG    = "pgdg"    // repmgr      → postgresqlNN-* from PGDG
	pgFlavorSource  = "source"  // spock       → patched PostgreSQL built into the same prefix
)

// aioPGFlavorLabel is how a flavor is named in a validation message.
var aioPGFlavorLabel = map[string]string{
	pgFlavorPercona: "Percona",
	pgFlavorPGDG:    "PGDG",
	pgFlavorSource:  "Spock (built from source)",
}

// aioPGFlavorOfKind is the PostgreSQL distribution a kind needs.
func aioPGFlavorOfKind(kind string) string {
	switch kind {
	case "pg", "patroni":
		return pgFlavorPercona
	case "repmgr":
		return pgFlavorPGDG
	case "spock":
		return pgFlavorSource
	}
	return ""
}

// aioPGFlavorConflicts returns, per PostgreSQL major, the instance names that
// disagree about which distribution to install. An empty result means the design
// is installable.
func aioPGFlavorConflicts(instances []aioInstance) map[string]map[string][]string {
	byMajor := map[string]map[string][]string{}
	for _, in := range instances {
		f := aioPGFlavorOfKind(in.Kind)
		if f == "" {
			continue
		}
		m := aioPGMajor(in)
		if byMajor[m] == nil {
			byMajor[m] = map[string][]string{}
		}
		byMajor[m][f] = append(byMajor[m][f], in.Name)
	}
	for m, flavors := range byMajor {
		if len(flavors) < 2 {
			delete(byMajor, m)
		}
	}
	return byMajor
}

// ---------------------------------------------------------------- repmgr

// aioRepmgrPackages is the PGDG package set for one major.
func aioRepmgrPackages(major string) string {
	return fmt.Sprintf("postgresql%s-server postgresql%s-contrib repmgr_%s", major, major, major)
}

// aioRepmgrConfPath is the instance's own repmgr.conf. The PGDG package puts one
// at /etc/repmgr/NN/repmgr.conf, but that is a single path per major — every
// repmgr instance in this container would share it, and every repmgr subcommand
// takes -f anyway.
func aioRepmgrConfPath(l instLayout) string { return l.ConfDir + "/repmgr.conf" }

// aioRepmgrConf renders one member's repmgr.conf.
//
// Two things differ from the classic node and both are load-bearing:
//
//   - conninfo carries an explicit port. Every member is 127.0.0.1 here, so the
//     port is the only thing identifying a node; repmgr would otherwise dial 5432
//     and find nothing.
//   - service_*_command point at this instance's OWN systemd unit. repmgr shells
//     out to these to restart PostgreSQL during a promote or follow, and its
//     defaults assume the packaged postgresql-NN unit — which is masked here, so
//     failover would appear to work and then leave the node down.
func aioRepmgrConf(l instLayout, m aioInstanceRuntime, nodeID int, bindir string, sec pgSecrets) string {
	conf := aioRepmgrConfPath(l)
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — repmgr instance %s. Generated.\n", m.Inst)
	fmt.Fprintf(&b, "node_id=%d\n", nodeID)
	fmt.Fprintf(&b, "node_name='%s'\n", m.Inst)
	fmt.Fprintf(&b, "conninfo='host=127.0.0.1 port=%d user=%s dbname=repmgr password=%s connect_timeout=2'\n",
		m.Ports.Client, sec.ReplUser, sec.ReplPassword)
	fmt.Fprintf(&b, "data_directory='%s'\n", l.DataDir)
	fmt.Fprintf(&b, "pg_bindir='%s'\n", bindir)
	fmt.Fprintf(&b, "repmgrd_pid_file='%s/repmgrd.pid'\n", l.RunDir)
	fmt.Fprintf(&b, "log_file='%s/repmgrd.log'\n", l.LogDir)
	// repmgr drives PostgreSQL through these during promote/follow.
	fmt.Fprintf(&b, "service_start_command='sudo systemctl start %s'\n", l.Unit)
	fmt.Fprintf(&b, "service_stop_command='sudo systemctl stop %s'\n", l.Unit)
	fmt.Fprintf(&b, "service_restart_command='sudo systemctl restart %s'\n", l.Unit)
	fmt.Fprintf(&b, "service_reload_command='sudo systemctl reload %s'\n", l.Unit)
	b.WriteString("failover='automatic'\n")
	fmt.Fprintf(&b, "promote_command='%s/repmgr standby promote -f %s --log-to-file'\n", bindir, conf)
	fmt.Fprintf(&b, "follow_command='%s/repmgr standby follow -f %s --log-to-file --upstream-node-id=%%n'\n", bindir, conf)
	b.WriteString("reconnect_attempts=6\nreconnect_interval=10\nmonitoring_history=yes\n")
	return b.String()
}

// aioProvisionRepmgr brings up one repmgr cluster: initdb + register the primary,
// then clone and register each standby from it.
func (a *App) aioProvisionRepmgr(ctx context.Context, id string, n designNode, in aioInstance, cfg aioConfig, fresh map[string]bool, pr *pxcProg, base, span int) error {
	major := aioPGMajor(in)
	bindir := pgBinDir(n.OS, major)
	group := aioSanitizeInst(in.Name)
	var members []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Kind == "repmgr" && m.Group == group {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return nil
	}
	// Same .env-derived credentials every other dbcanvas PostgreSQL engine uses,
	// so accounts are identical across classic and All-in-One nodes.
	sec := pgFamilySecrets()
	if pw := strings.TrimSpace(in.RootPassword); pw != "" {
		sec.SuperPassword = pw
	}

	for i, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		primary := members[0]
		role := "standby"
		if i == 0 {
			role = "primary"
		}
		pr.phase(fmt.Sprintf("Preparing repmgr %s member %s (%s, %d/%d)", in.Name, m.Inst, role, i+1, len(members)), base+span/2)

		if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
			return fmt.Errorf("%s: create directories: %w", m.Inst, err)
		}
		if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "repmgr.conf", 0o640,
			[]byte(aioRepmgrConf(l, m, i+1, bindir, sec))); err != nil {
			return fmt.Errorf("%s: write repmgr.conf: %w", m.Inst, err)
		}
		// CopyFile writes as root; every repmgr subcommand runs as postgres via
		// runuser, so without this the clone fails with "unable to open provided
		// configuration file" — which then surfaced as a confusing sed error.
		if err := a.runStep(ctx, id, aioRepmgrConfOwnScript,
			[]string{"CONF=" + aioRepmgrConfPath(l)}, pr.logln); err != nil {
			return fmt.Errorf("%s: own repmgr.conf: %w", m.Inst, err)
		}

		if i == 0 {
			// Primary: initdb, then the same instance-scoped config the pg kind
			// gets plus the replication settings repmgr needs.
			if err := a.runStep(ctx, id, aioPGInitScript, []string{
				"BINDIR=" + bindir, "DATADIR=" + l.DataDir, "LOGDIR=" + l.LogDir,
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: initdb: %w", m.Inst, err)
			}
			if err := a.runStep(ctx, id, aioRepmgrPrimaryConfigScript, []string{
				"DATADIR=" + l.DataDir, "RUNDIR=" + l.RunDir,
				fmt.Sprintf("PORT=%d", m.Ports.Client),
				"REPLUSER=" + sec.ReplUser,
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: configure: %w", m.Inst, err)
			}
		} else {
			// Standby: repmgr clones the datadir straight from the primary, so
			// there is no initdb and no config to write — the clone brings the
			// primary's, and repmgr rewrites what it must.
			if err := a.runStep(ctx, id, aioRepmgrCloneScript, []string{
				"BINDIR=" + bindir, "DATADIR=" + l.DataDir, "LOGDIR=" + l.LogDir,
				"CONF=" + aioRepmgrConfPath(l),
				fmt.Sprintf("PRIMARY_PORT=%d", primary.Ports.Client),
				"REPLUSER=" + sec.ReplUser, "REPLPW=" + sec.ReplPassword,
				fmt.Sprintf("PORT=%d", m.Ports.Client), "RUNDIR=" + l.RunDir,
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: repmgr standby clone: %w", m.Inst, err)
			}
		}

		if err := a.aioPGWriteUnit(ctx, id, l, m, bindir, "repmgr"); err != nil {
			return err
		}
		if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
			return fmt.Errorf("%s: start: %w", m.Inst, err)
		}

		if i == 0 {
			if err := a.runStep(ctx, id, aioRepmgrPrimaryRegisterScript, []string{
				"BINDIR=" + bindir, "RUNDIR=" + l.RunDir, "CONF=" + aioRepmgrConfPath(l),
				fmt.Sprintf("PORT=%d", m.Ports.Client),
				"REPLUSER=" + sec.ReplUser, "REPLPW=" + sec.ReplPassword, "PGPW=" + sec.SuperPassword,
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: repmgr primary register: %w", m.Inst, err)
			}
			pr.logln(fmt.Sprintf("%s registered as repmgr primary (node 1, port %d)", m.Inst, m.Ports.Client))
		} else {
			if err := a.runStep(ctx, id, aioRepmgrStandbyRegisterScript, []string{
				"BINDIR=" + bindir, "RUNDIR=" + l.RunDir, "CONF=" + aioRepmgrConfPath(l),
				fmt.Sprintf("PORT=%d", m.Ports.Client),
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: repmgr standby register: %w", m.Inst, err)
			}
			pr.logln(fmt.Sprintf("%s cloned and registered as repmgr standby (node %d, port %d)", m.Inst, i+1, m.Ports.Client))
		}

		// repmgrd gives the cluster automatic failover. Its own unit per instance:
		// repmgrd daemonizes, so this is Type=forking with an explicit PIDFile —
		// a Type=simple unit would see ExecStart exit 0 and mark it dead.
		if err := a.aioRepmgrdUnit(ctx, id, l, bindir); err != nil {
			pr.logln(m.Inst + ": repmgrd not started (failover disabled): " + err.Error())
		}
	}
	return nil
}

// aioRepmgrdUnit writes and starts one instance's repmgrd unit.
func (a *App) aioRepmgrdUnit(ctx context.Context, id string, l instLayout, bindir string) error {
	unit := l.Unit + "-repmgrd"
	body := fmt.Sprintf(`[Unit]
Description=dbcanvas All-in-One repmgrd for %s
PartOf=%s
After=%s.service
Requires=%s.service

[Service]
Type=forking
User=postgres
Group=postgres
RuntimeDirectory=aio/%s
RuntimeDirectoryPreserve=yes
PIDFile=%s/repmgrd.pid
ExecStart=%s/repmgrd -f %s --pid-file=%s/repmgrd.pid
Restart=no

[Install]
WantedBy=%s
`, l.Inst, aioTarget, l.Unit, l.Unit, l.Inst, l.RunDir, bindir, aioRepmgrConfPath(l), l.RunDir, aioTarget)
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/systemd/system", unit+".service", 0o644, []byte(body)); err != nil {
		return err
	}
	return a.runStep(ctx, id, "systemctl daemon-reload && systemctl enable --now "+unit+" >/dev/null 2>&1", nil, func(string) {})
}

// ------------------------------------------------------------------ scripts

// aioRepmgrPrimaryConfigScript is aioPGConfigureScript plus the WAL settings
// repmgr needs, and a pg_hba that lets the repmgr user replicate over TCP.
const aioRepmgrPrimaryConfigScript = `set -e
CONF="$DATADIR/postgresql.conf"
HBA="$DATADIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
sed -i '/^# --- dbcanvas ---$/,$d' "$CONF"
{
  echo "# --- dbcanvas ---"
  echo "listen_addresses = '*'"
  echo "port = $PORT"
  echo "unix_socket_directories = '$RUNDIR'"
  echo "password_encryption = scram-sha-256"
  echo "wal_level = replica"
  echo "max_wal_senders = 10"
  echo "max_replication_slots = 10"
  echo "wal_keep_size = 512MB"
  echo "hot_standby = on"
  echo "archive_mode = off"
  echo "shared_preload_libraries = 'repmgr'"
} >> "$CONF"
grep -q "dbcanvas-aio-repmgr" "$HBA" || {
  echo "# dbcanvas-aio-repmgr" >> "$HBA"
  echo "local   replication   $REPLUSER                    trust"        >> "$HBA"
  echo "host    replication   $REPLUSER   127.0.0.1/32     trust"        >> "$HBA"
  echo "local   repmgr        $REPLUSER                    trust"        >> "$HBA"
  echo "host    repmgr        $REPLUSER   127.0.0.1/32     trust"        >> "$HBA"
  echo "host    all           all         0.0.0.0/0        scram-sha-256" >> "$HBA"
}
chown postgres:postgres "$CONF" "$HBA"
exit 0`

// aioRepmgrPrimaryRegisterScript creates the repmgr role + database and registers
// the primary. Idempotent: a re-run finds both already there.
const aioRepmgrPrimaryRegisterScript = `set -e
P() { runuser -u postgres -- "$BINDIR/psql" -h "$RUNDIR" -p "$PORT" -U postgres "$@"; }
for i in $(seq 1 30); do
  runuser -u postgres -- "$BINDIR/pg_isready" -h "$RUNDIR" -p "$PORT" >/dev/null 2>&1 && break
  sleep 1
done
P -v ON_ERROR_STOP=1 -qtAc "ALTER USER postgres WITH PASSWORD '$PGPW';" >/dev/null
P -qtAc "SELECT 1 FROM pg_roles WHERE rolname='$REPLUSER'" | grep -q 1 || \
  P -v ON_ERROR_STOP=1 -qtAc "CREATE ROLE $REPLUSER WITH LOGIN REPLICATION SUPERUSER PASSWORD '$REPLPW';" >/dev/null
P -qtAc "SELECT 1 FROM pg_database WHERE datname='repmgr'" | grep -q 1 || \
  P -v ON_ERROR_STOP=1 -qtAc "CREATE DATABASE repmgr OWNER $REPLUSER;" >/dev/null
runuser -u postgres -- "$BINDIR/repmgr" -f "$CONF" primary register -F 2>&1 | tail -10
exit 0`

// aioRepmgrCloneScript clones a standby's datadir from the primary, then pins the
// clone's own port and socket directory — the clone copies the PRIMARY's
// postgresql.conf, so without this the standby would try to bind the primary's
// port and fail to start.
const aioRepmgrCloneScript = `set -e
install -d -m 0700 -o postgres -g postgres "$DATADIR"
install -d -m 0750 -o postgres -g postgres "$LOGDIR"
if [ ! -s "$DATADIR/PG_VERSION" ]; then
  find "$DATADIR" -mindepth 1 -delete 2>/dev/null || true
  # NOT piped into tail: a pipeline exits with the LAST command's status, so
  # "| tail" would swallow a clone failure and let the config rewrite below run
  # against a datadir that does not exist.
  OUT=$(runuser -u postgres -- env PGPASSWORD="$REPLPW" "$BINDIR/repmgr" \
    -h 127.0.0.1 -p "$PRIMARY_PORT" -U "$REPLUSER" -d repmgr -f "$CONF" \
    standby clone --fast-checkpoint -F 2>&1) || { echo "$OUT" | tail -20; exit 1; }
  echo "$OUT" | tail -5
fi
[ -s "$DATADIR/PG_VERSION" ] || { echo "clone produced no datadir at $DATADIR"; exit 1; }
CONFFILE="$DATADIR/postgresql.conf"
sed -i '/^# --- dbcanvas standby ---$/,$d' "$CONFFILE"
{
  echo "# --- dbcanvas standby ---"
  echo "port = $PORT"
  echo "unix_socket_directories = '$RUNDIR'"
  echo "hot_standby = on"
} >> "$CONFFILE"
chown postgres:postgres "$CONFFILE"
exit 0`

// aioRepmgrConfOwnScript hands repmgr.conf to the postgres user.
const aioRepmgrConfOwnScript = `set -e
chown postgres:postgres "$CONF"
chmod 640 "$CONF"
exit 0`

// aioRepmgrStandbyRegisterScript registers the standby once it is streaming.
const aioRepmgrStandbyRegisterScript = `set -e
for i in $(seq 1 60); do
  runuser -u postgres -- "$BINDIR/pg_isready" -h "$RUNDIR" -p "$PORT" >/dev/null 2>&1 && break
  sleep 2
done
runuser -u postgres -- "$BINDIR/repmgr" -f "$CONF" standby register -F --wait-start=60 2>&1 | tail -10
exit 0`

// ---------------------------------------------------------------- Patroni

// Patroni is the most involved kind in this node type, because each member is
// really THREE things: an etcd member, a Patroni agent, and the PostgreSQL that
// Patroni starts. That last point drives the design — Patroni owns postgres
// (it runs initdb, writes postgresql.conf, and starts/stops the server), so
// unlike every other PostgreSQL kind here there is NO postgres unit of our own.
// The instance's unit IS Patroni, with a companion etcd unit alongside it, the
// same shape repmgrd already uses.
//
// The slot reserves exactly what a member needs: +0 postgres, +1 Patroni REST,
// +2 etcd client, +3 etcd peer. All three members are 127.0.0.1, so every one of
// those has to be explicit — etcd's own defaults (2379/2380) and Patroni's
// (8008) are per-host and the second member could not bind them.

// aioPatroniConfPath / aioEtcdConfPath are per-instance; the packaged locations
// (/etc/patroni/patroni.yml, /etc/etcd/etcd.yaml) are single-path-per-host and
// every member here would share them.
func aioPatroniConfPath(l instLayout) string { return l.ConfDir + "/patroni.yml" }
func aioEtcdConfPath(l instLayout) string    { return l.ConfDir + "/etcd.yaml" }

// aioEtcdDataDir keeps etcd's state inside the instance tree, beside PGDATA.
func aioEtcdDataDir(l instLayout) string { return l.Dir + "/etcd" }

// aioEtcdConf renders one member's etcd config. initial-cluster lists every peer
// by its own peer port — the only thing distinguishing them on a shared address.
func aioEtcdConf(l instLayout, m aioInstanceRuntime, cluster string, members []aioInstanceRuntime) string {
	var peers []string
	for _, p := range members {
		pl := aioLayout(p.Inst, p.Kind, p.Ports)
		peers = append(peers, fmt.Sprintf("%s=http://127.0.0.1:%d", pl.Inst, p.Ports.EtcdPr))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — etcd for %s. Generated.\n", m.Inst)
	fmt.Fprintf(&b, "name: %s\n", m.Inst)
	fmt.Fprintf(&b, "data-dir: %s\n", aioEtcdDataDir(l))
	fmt.Fprintf(&b, "listen-peer-urls: http://127.0.0.1:%d\n", m.Ports.EtcdPr)
	fmt.Fprintf(&b, "listen-client-urls: http://127.0.0.1:%d\n", m.Ports.EtcdCli)
	fmt.Fprintf(&b, "initial-advertise-peer-urls: http://127.0.0.1:%d\n", m.Ports.EtcdPr)
	fmt.Fprintf(&b, "advertise-client-urls: http://127.0.0.1:%d\n", m.Ports.EtcdCli)
	fmt.Fprintf(&b, "initial-cluster: %s\n", strings.Join(peers, ","))
	fmt.Fprintf(&b, "initial-cluster-token: %s\n", cluster)
	b.WriteString("initial-cluster-state: new\n")
	return b.String()
}

// aioPatroniYAML renders one member's patroni.yml.
func aioPatroniYAML(l instLayout, m aioInstanceRuntime, in aioInstance, os string, members []aioInstanceRuntime, sec pgSecrets) string {
	major := aioPGMajor(in)
	var endpoints []string
	for _, p := range members {
		endpoints = append(endpoints, fmt.Sprintf("127.0.0.1:%d", p.Ports.EtcdCli))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — Patroni for %s. Generated.\n", m.Inst)
	fmt.Fprintf(&b, "scope: %s\n", m.Group)
	b.WriteString("namespace: /dbcanvas/\n")
	fmt.Fprintf(&b, "name: %s\n\n", m.Inst)

	// Every member is 127.0.0.1, so connect_address must carry the port or the
	// leader advertises an address its peers cannot distinguish.
	fmt.Fprintf(&b, "restapi:\n  listen: 0.0.0.0:%d\n  connect_address: 127.0.0.1:%d\n\n",
		m.Ports.REST, m.Ports.REST)
	b.WriteString("etcd3:\n  hosts:\n")
	for _, e := range endpoints {
		fmt.Fprintf(&b, "  - %s\n", e)
	}
	b.WriteString("\n")

	b.WriteString("bootstrap:\n  dcs:\n    ttl: 30\n    loop_wait: 10\n    retry_timeout: 10\n")
	b.WriteString("    maximum_lag_on_failover: 1048576\n")
	b.WriteString("    postgresql:\n      use_pg_rewind: true\n      use_slots: true\n")
	b.WriteString("      parameters:\n        max_connections: 200\n        hot_standby: \"on\"\n        wal_level: replica\n")
	b.WriteString("  initdb:\n  - encoding: UTF8\n  - data-checksums\n")
	b.WriteString("  pg_hba:\n  - local all all trust\n  - host all all 127.0.0.1/32 trust\n")
	b.WriteString("  - host all all 0.0.0.0/0 scram-sha-256\n")
	fmt.Fprintf(&b, "  - host replication %s 0.0.0.0/0 scram-sha-256\n\n", sec.ReplUser)

	fmt.Fprintf(&b, "postgresql:\n  listen: 0.0.0.0:%d\n  connect_address: 127.0.0.1:%d\n",
		m.Ports.Client, m.Ports.Client)
	fmt.Fprintf(&b, "  data_dir: %s\n", l.DataDir)
	fmt.Fprintf(&b, "  bin_dir: %s\n", pgBinDir(os, major))
	// Per-instance pgpass: Patroni rewrites this file, and a shared /tmp/pgpass
	// would have three agents racing on it.
	fmt.Fprintf(&b, "  pgpass: %s/pgpass\n", l.RunDir)
	fmt.Fprintf(&b, "  parameters:\n    unix_socket_directories: '%s'\n", l.RunDir)
	b.WriteString("  authentication:\n    superuser:\n")
	fmt.Fprintf(&b, "      username: %s\n      password: \"%s\"\n", sec.SuperUser, yamlEscape(sec.SuperPassword))
	b.WriteString("    replication:\n")
	fmt.Fprintf(&b, "      username: %s\n      password: \"%s\"\n", sec.ReplUser, yamlEscape(sec.ReplPassword))
	b.WriteString("\ntags:\n  nofailover: false\n  noloadbalance: false\n  clonefrom: false\n  nosync: false\n")
	return b.String()
}

// aioProvisionPatroni brings up one Patroni cluster: every etcd member first (a
// quorum has to exist before any Patroni agent can elect a leader), then the
// Patroni agents, which create and manage PostgreSQL themselves.
func (a *App) aioProvisionPatroni(ctx context.Context, id string, n designNode, in aioInstance, cfg aioConfig, fresh map[string]bool, pr *pxcProg, base, span int) error {
	group := aioSanitizeInst(in.Name)
	var members []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Kind == "patroni" && m.Group == group {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return nil
	}
	sec := pgFamilySecrets()
	if pw := strings.TrimSpace(in.RootPassword); pw != "" {
		sec.SuperPassword = pw
	}

	// Phase 1: the whole etcd cluster. Patroni cannot elect a leader without a
	// DCS quorum, so all etcd members go up before any agent starts.
	for _, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		pr.phase("Starting etcd for "+m.Inst, base+span/4)
		if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
			return fmt.Errorf("%s: create directories: %w", m.Inst, err)
		}
		if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "etcd.yaml", 0o644,
			[]byte(aioEtcdConf(l, m, group, members))); err != nil {
			return fmt.Errorf("%s: write etcd.yaml: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioEtcdDirScript,
			[]string{"DATADIR=" + aioEtcdDataDir(l)}, pr.logln); err != nil {
			return fmt.Errorf("%s: prepare etcd datadir: %w", m.Inst, err)
		}
		if err := a.aioEtcdUnit(ctx, id, l); err != nil {
			return fmt.Errorf("%s: etcd unit: %w", m.Inst, err)
		}
	}
	// Start every etcd at once and non-blocking: none of them reports ready until
	// the cluster has a quorum, so starting them one at a time deadlocks.
	var etcdUnits []string
	for _, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		etcdUnits = append(etcdUnits, aioLayout(m.Inst, m.Kind, m.Ports).Unit+"-etcd")
	}
	if len(etcdUnits) > 0 {
		if err := a.runStep(ctx, id, aioEtcdStartScript,
			[]string{"UNITS=" + strings.Join(etcdUnits, " ")}, pr.logln); err != nil {
			return fmt.Errorf("start etcd cluster: %w", err)
		}
	}
	for _, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		if err := a.runStep(ctx, id, aioEtcdWaitScript, []string{
			fmt.Sprintf("PORT=%d", m.Ports.EtcdCli), "INST=" + m.Inst,
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: etcd did not become healthy: %w", m.Inst, err)
		}
	}
	pr.logln(fmt.Sprintf("etcd cluster for %s healthy (%d member(s))", in.Name, len(members)))

	// Phase 2: the Patroni agents. Patroni runs initdb and starts PostgreSQL, so
	// there is deliberately no postgres unit of our own here.
	for i, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		pr.phase(fmt.Sprintf("Starting Patroni %s (%d/%d)", m.Inst, i+1, len(members)), base+span/2)
		if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "patroni.yml", 0o640,
			[]byte(aioPatroniYAML(l, m, in, n.OS, members, sec))); err != nil {
			return fmt.Errorf("%s: write patroni.yml: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioPatroniOwnScript, []string{
			"CONF=" + aioPatroniConfPath(l), "DATADIR=" + l.DataDir,
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: own patroni.yml: %w", m.Inst, err)
		}
		if err := a.aioPatroniUnit(ctx, id, l); err != nil {
			return fmt.Errorf("%s: patroni unit: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioStartUnitScript,
			[]string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
			return fmt.Errorf("%s: start patroni: %w", m.Inst, err)
		}
		// The leader must be up before the next member joins, or two agents race
		// to bootstrap the same scope.
		if err := a.runStep(ctx, id, aioPatroniWaitScript, []string{
			fmt.Sprintf("REST=%d", m.Ports.REST), "INST=" + m.Inst, "LOGERR=" + l.LogErr,
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: patroni did not become healthy: %w", m.Inst, err)
		}
		pr.logln(fmt.Sprintf("%s running (postgres :%d, REST :%d, etcd :%d/%d)",
			m.Inst, m.Ports.Client, m.Ports.REST, m.Ports.EtcdCli, m.Ports.EtcdPr))
	}
	return nil
}

// aioEtcdUnit writes and starts one member's etcd unit.
func (a *App) aioEtcdUnit(ctx context.Context, id string, l instLayout) error {
	unit := l.Unit + "-etcd"
	body := fmt.Sprintf(`[Unit]
Description=dbcanvas All-in-One etcd for %s
PartOf=%s
Before=%s.service

[Service]
Type=notify
User=postgres
Group=postgres
RuntimeDirectory=aio/%s
RuntimeDirectoryPreserve=yes
ExecStart=/usr/bin/etcd --config-file %s
TimeoutStartSec=120
Restart=no

[Install]
WantedBy=%s
`, l.Inst, aioTarget, l.Unit, l.Inst, aioEtcdConfPath(l), aioTarget)
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/systemd/system", unit+".service", 0o644, []byte(body)); err != nil {
		return err
	}
	// Write and enable ONLY. etcd is Type=notify and does not report ready until
	// it has a quorum, so `enable --now` on the first member blocks until its
	// start times out — before the other members even exist. They are started
	// together, non-blocking, by aioEtcdStartScript.
	return a.runStep(ctx, id,
		"systemctl daemon-reload && systemctl reset-failed "+unit+" 2>/dev/null; systemctl enable "+unit+" >/dev/null 2>&1 || true",
		nil, func(string) {})
}

// aioPatroniUnit writes one member's Patroni unit. Patroni starts PostgreSQL
// itself, so this unit is the instance — there is no separate postgres unit, and
// `aioctl stop <inst>` correctly takes the database down with the agent.
func (a *App) aioPatroniUnit(ctx context.Context, id string, l instLayout) error {
	return a.aioWriteUnit(ctx, id, l, aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One Patroni instance %s", l.Inst),
		ExecStart:   fmt.Sprintf("/usr/bin/patroni %s", aioPatroniConfPath(l)),
		Type:        "simple",
		User:        "postgres",
		Group:       "postgres",
		TimeoutSec:  300,
		After:       []string{l.Unit + "-etcd.service"},
		Requires:    []string{l.Unit + "-etcd.service"},
		EnvFile: fmt.Sprintf("AIO_INST=%s\nAIO_KIND=patroni\nAIO_PORT=%d\nAIO_SOCKET=%s\nAIO_DATADIR=%s\nAIO_CNF=%s\nPGBIN=%s\n",
			l.Inst, l.Ports.Client, l.RunDir, l.DataDir, aioPatroniConfPath(l), "/usr/bin"),
	})
}

// ------------------------------------------------------------------ scripts

const aioEtcdDirScript = `set -e
install -d -m 0700 -o postgres -g postgres "$DATADIR"
exit 0`

// aioPatroniOwnScript hands the config and PGDATA parent to postgres. Patroni
// runs as postgres and creates PGDATA itself, so only the parent must exist.
const aioPatroniOwnScript = `set -e
chown postgres:postgres "$CONF"; chmod 640 "$CONF"
install -d -m 0700 -o postgres -g postgres "$DATADIR"
exit 0`

// aioEtcdStartScript launches every member's etcd together with --no-block.
// Blocking would deadlock: Type=notify readiness requires a quorum, and a quorum
// requires the other members to already be running.
const aioEtcdStartScript = `set -e
systemctl daemon-reload
for u in $UNITS; do systemctl reset-failed "$u" 2>/dev/null || true; done
for u in $UNITS; do systemctl start --no-block "$u" 2>/dev/null || true; done
exit 0`

const aioEtcdWaitScript = `set -e
OK=0
for i in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$PORT/health" 2>/dev/null | grep -q '"health":"true"' && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "$INST: etcd on $PORT is not healthy"; exit 1; }
exit 0`

// aioPatroniWaitScript waits for the agent's REST API to report a settled role.
// "running" covers a leader; a replica reports "streaming" once it has caught up.
const aioPatroniWaitScript = `set -e
OK=0
for i in $(seq 1 90); do
  S=$(curl -s "http://127.0.0.1:$REST/patroni" 2>/dev/null | tr ',' '\n' | grep -o '"state": *"[a-z]*"' | head -1 | sed 's/.*"\([a-z]*\)"$/\1/')
  case "$S" in running|streaming) OK=1; break;; esac
  sleep 2
done
if [ "$OK" != 1 ]; then
  echo "$INST: patroni state is ${S:-unreachable}"
  curl -s "http://127.0.0.1:$REST/patroni" 2>/dev/null | head -c 400; echo
  tail -20 "$LOGERR" 2>/dev/null
  journalctl -u "aio-$INST" --no-pager -n 20 2>/dev/null | tail -20
  exit 1
fi
exit 0`

// ---------------------------------------------------------------- Spock

// Spock is the only kind that BUILDS rather than installs: pgEdge's extension
// needs a patched PostgreSQL, so spock.go compiles postgres from source, applies
// the patches, and installs the pair into /usr/pgsql-NN.
//
// That works in an All-in-One node's favour. The build is per MAJOR, not per
// member, so three Spock members share one compile — the expensive part — and
// differ only in datadir, port and socket. It is guarded on spock.so already
// existing, so a redeploy skips it entirely.
//
// The topology is a full mesh of logical-replication subscriptions: every node
// subscribes to every other. Each node's DSN is 127.0.0.1 plus its own port —
// the peers are otherwise indistinguishable.

// aioSpockDB is the demo database the mesh replicates.
const aioSpockDB = "spockdemo"

// aioSpockDSN is how peers reach one member.
func aioSpockDSN(m aioInstanceRuntime, sec pgSecrets) string {
	return fmt.Sprintf("host=127.0.0.1 port=%d dbname=%s user=%s password=%s",
		m.Ports.Client, aioSpockDB, sec.SuperUser, sec.SuperPassword)
}

// aioProvisionSpock builds PostgreSQL+Spock once, brings up each member on its
// own port, then wires the full mesh.
func (a *App) aioProvisionSpock(ctx context.Context, id string, n designNode, in aioInstance, cfg aioConfig, fresh map[string]bool, pr *pxcProg, base, span int) error {
	group := aioSanitizeInst(in.Name)
	var members []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Kind == "spock" && m.Group == group {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return nil
	}
	major := aioPGMajor(in)
	prefix := "/usr/pgsql-" + major
	sec := pgFamilySecrets()
	if pw := strings.TrimSpace(in.RootPassword); pw != "" {
		sec.SuperPassword = pw
	}

	// One build serves every member of this major. Guarded on spock.so, so a
	// redeploy — or a second Spock cluster on the same major — skips it.
	pr.phase("Installing Spock build toolchain", base)
	if err := a.runStep(ctx, id, spockBuildDepsRHEL,
		[]string{"EPELPKG=" + epelPackage(n.OSVersion)}, pr.logln); err != nil {
		return fmt.Errorf("install Spock build dependencies: %w", err)
	}
	pr.phase(fmt.Sprintf("Compiling PostgreSQL %s + Spock (this takes a while)", major), base+span/6)
	if err := a.runStep(ctx, id, spockCompileScript, []string{
		"PGMAJOR=" + major, "PGREF=" + spockPGRef(major, in.PGVersion),
		"SPOCK_REF=" + spockRef(), "PREFIX=" + prefix,
	}, pr.logln); err != nil {
		return fmt.Errorf("build PostgreSQL %s + Spock: %w", major, err)
	}
	pr.logln("PostgreSQL " + major + " + Spock built and installed to " + prefix)

	for i, m := range members {
		if !fresh[m.Inst] {
			continue
		}
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		pr.phase(fmt.Sprintf("Preparing Spock member %s (%d/%d)", m.Inst, i+1, len(members)), base+span/2)
		if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
			return fmt.Errorf("%s: create directories: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioPGInitScript, []string{
			"BINDIR=" + prefix + "/bin", "DATADIR=" + l.DataDir, "LOGDIR=" + l.LogDir,
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: initdb: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioSpockConfigScript, []string{
			"DATADIR=" + l.DataDir, "RUNDIR=" + l.RunDir,
			fmt.Sprintf("PORT=%d", m.Ports.Client),
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: configure: %w", m.Inst, err)
		}
		if err := a.aioPGWriteUnit(ctx, id, l, m, prefix+"/bin", "Spock"); err != nil {
			return err
		}
		if err := a.runStep(ctx, id, aioStartUnitScript,
			[]string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
			return fmt.Errorf("%s: start: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioPGPasswordScript, []string{
			"BINDIR=" + prefix + "/bin", "RUNDIR=" + l.RunDir,
			fmt.Sprintf("PORT=%d", m.Ports.Client), "PGPW=" + sec.SuperPassword,
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: set superuser password: %w", m.Inst, err)
		}
		if err := a.runStep(ctx, id, aioSpockNodeSetupScript, []string{
			"BINDIR=" + prefix + "/bin", "RUNDIR=" + l.RunDir,
			fmt.Sprintf("PORT=%d", m.Ports.Client),
			"DB=" + aioSpockDB, "NODE=" + m.Inst, "DSN=" + aioSpockDSN(m, sec),
		}, pr.logln); err != nil {
			return fmt.Errorf("%s: spock node setup: %w", m.Inst, err)
		}
		pr.logln(fmt.Sprintf("%s running on port %d (spock node %s)", m.Inst, m.Ports.Client, m.Inst))
	}

	// Full mesh, after every node exists: a subscription needs its provider up
	// and already node_create'd.
	pr.phase("Wiring the Spock mesh for "+in.Name, base+(span*5)/6)
	subs := 0
	for _, me := range members {
		l := aioLayout(me.Inst, me.Kind, me.Ports)
		for _, peer := range members {
			if peer.Inst == me.Inst {
				continue
			}
			if err := a.runStep(ctx, id, aioSpockSubCreateScript, []string{
				"BINDIR=" + prefix + "/bin", "RUNDIR=" + l.RunDir,
				fmt.Sprintf("PORT=%d", me.Ports.Client),
				"DB=" + aioSpockDB,
				"SUB=" + fmt.Sprintf("sub_%s_%s", strings.ReplaceAll(me.Inst, "-", "_"), strings.ReplaceAll(peer.Inst, "-", "_")),
				"DSN=" + aioSpockDSN(peer, sec),
			}, pr.logln); err != nil {
				return fmt.Errorf("%s: subscribe to %s: %w", me.Inst, peer.Inst, err)
			}
			subs++
		}
	}
	pr.logln(fmt.Sprintf("spock mesh for %s complete (%d subscription(s) across %d node(s))",
		in.Name, subs, len(members)))
	return nil
}

// ------------------------------------------------------------------ scripts

// aioSpockConfigScript pins the instance's port and socket and enables what Spock
// needs (logical WAL, the spock library, commit timestamps). Replaced rather than
// appended on redeploy, like the pg kind's.
const aioSpockConfigScript = `set -e
CONF="$DATADIR/postgresql.conf"
HBA="$DATADIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
sed -i '/^# --- dbcanvas spock ---$/,$d' "$CONF"
{
  echo "# --- dbcanvas spock ---"
  echo "listen_addresses = '*'"
  echo "port = $PORT"
  echo "unix_socket_directories = '$RUNDIR'"
  echo "password_encryption = scram-sha-256"
  echo "wal_level = logical"
  echo "shared_preload_libraries = 'spock'"
  echo "track_commit_timestamp = on"
  echo "max_worker_processes = 16"
  echo "max_replication_slots = 16"
  echo "max_wal_senders = 16"
} >> "$CONF"
grep -q "dbcanvas-aio-spock" "$HBA" || {
  echo "# dbcanvas-aio-spock" >> "$HBA"
  echo "host all all 127.0.0.1/32 scram-sha-256" >> "$HBA"
  echo "host replication all 127.0.0.1/32 scram-sha-256" >> "$HBA"
  echo "host all all 0.0.0.0/0 scram-sha-256" >> "$HBA"
}
chown postgres:postgres "$CONF" "$HBA"
exit 0`

// aioSpockNodeSetupScript creates the demo database, the extension and this
// node's spock node record. Idempotent at every step.
const aioSpockNodeSetupScript = `set -e
PSQL() { runuser -u postgres -- "$BINDIR/psql" -h "$RUNDIR" -p "$PORT" -U postgres "$@"; }
DB_() { PSQL -v ON_ERROR_STOP=1 -d "$DB" "$@"; }
for i in $(seq 1 30); do
  runuser -u postgres -- "$BINDIR/pg_isready" -h "$RUNDIR" -p "$PORT" >/dev/null 2>&1 && break
  sleep 1
done
PSQL -tAc "SELECT 1 FROM pg_database WHERE datname='$DB'" | grep -q 1 || \
  runuser -u postgres -- "$BINDIR/createdb" -h "$RUNDIR" -p "$PORT" -U postgres "$DB"
DB_ -c "CREATE EXTENSION IF NOT EXISTS spock;"
EXISTS=$(DB_ -tAc "SELECT count(*) FROM spock.node WHERE node_name = '$NODE'")
if [ "$EXISTS" = 0 ]; then
  printf '%s\n' "SELECT spock.node_create(node_name := :'node', dsn := :'dsn');" | DB_ -v node="$NODE" -v dsn="$DSN"
fi
DB_ -c "CREATE TABLE IF NOT EXISTS public.spock_demo (id bigint PRIMARY KEY, note text, updated_at timestamptz DEFAULT now());"
DB_ -c "SELECT spock.repset_add_all_tables('default', ARRAY['public']);" 2>/dev/null || true
exit 0`

// aioSpockSubCreateScript creates one edge of the mesh. forward_origins='{}'
// forwards only local-origin changes, so a write does not loop the ring.
const aioSpockSubCreateScript = `set -e
DB_() { runuser -u postgres -- "$BINDIR/psql" -h "$RUNDIR" -p "$PORT" -U postgres -v ON_ERROR_STOP=1 -d "$DB" "$@"; }
EXISTS=$(DB_ -tAc "SELECT count(*) FROM spock.subscription WHERE sub_name = '$SUB'")
if [ "$EXISTS" = 0 ]; then
  printf '%s\n' "SELECT spock.sub_create(subscription_name := :'sub', provider_dsn := :'dsn', replication_sets := ARRAY['default','default_insert_only','ddl_sql'], synchronize_structure := false, synchronize_data := false, forward_origins := '{}');" | DB_ -v sub="$SUB" -v dsn="$DSN"
fi
exit 0`
