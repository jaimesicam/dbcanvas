package main

import (
	"context"
	"fmt"
	"strings"
)

// aio_mongo.go — MongoDB instances inside an All-in-One node.
//
// Kinds: psmdb (standalone), psmrs (replica set), psmdbsharded (config RS +
// shard RS's + a mongos router).
//
// MongoDB is the cheapest family to co-tenant and the clearest illustration of
// the difference between the two kinds of packaging constraint this node type
// has to live with:
//
//   - Percona's PSMDB packages are UNVERSIONED (percona-server-mongodb-server,
//     -mongos, -tools, percona-mongodb-mongosh) and the major is chosen purely by
//     repo (psmdb-60/70/80), with binaries at /usr/bin/mongod. So one PSMDB
//     version per container — a shared *version*.
//   - But that costs nothing in capability. One install of -server plus -mongos
//     yields every topology at once, as many as you like, because they are all
//     just mongod/mongos processes with different config files. Unlike the MySQL
//     flavor conflict, there is nothing to forbid here.
//
// The classic path installs -server OR -mongos (mongodb.go:846/848) because each
// node is single-purpose; an All-in-One node installs both.
//
// Also unlike the classic path: PSMDB 6.0/7.0 ship a Type=forking mongod unit
// that misbehaves against the code's fork:false (mongodb.go:1332). Instances here
// run dbcanvas-authored Type=simple units, so that quirk does not arise.

// aioPSMDBMajor is the node-level PSMDB series. One install serves every MongoDB
// instance, so this is a node choice, not a per-instance one.
func aioPSMDBMajor(n designNode) string {
	m := strings.TrimSpace(n.AIOPSMDBMajor)
	if m == "" {
		return "8.0"
	}
	return m
}

// aioProvisionMongo installs the PSMDB package set once, then brings up every
// MongoDB instance and initiates any replica sets / sharded clusters.
func (a *App) aioProvisionMongo(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg, base, span int) error {
	major := aioPSMDBMajor(n)
	pr.phase("Installing PS MongoDB "+major, base)

	// Both -server and -mongos: an All-in-One node may need mongod and a router
	// in the same container, which no single-purpose node ever does.
	pkgs := "percona-server-mongodb-server percona-server-mongodb-mongos " +
		"percona-server-mongodb-tools percona-mongodb-mongosh"
	instScript := mongoInstallRHEL
	if isDebianOS(n.OS) {
		instScript = mongoInstallDebian
	}
	if err := a.runStep(ctx, id, instScript, []string{
		"PSMDB_REPO=" + psmdbRepo(major), "PKGS=" + pkgs, "VER=" + n.AIOPSMDBVersion,
	}, pr.logln); err != nil {
		return fmt.Errorf("install PS MongoDB: %w", err)
	}
	pr.logln("PS MongoDB " + major + " installed (mongod + mongos)")
	if err := a.runStep(ctx, id, aioMaskVendorUnits, []string{"UNITS=mongod mongos"}, pr.logln); err != nil {
		return fmt.Errorf("mask vendor mongod unit: %w", err)
	}
	pr.logln("vendor mongod unit masked — instances own their ports")

	// One keyFile for the whole node: every replica set and shard in this
	// container shares an internal-auth secret, exactly as a classic multi-node
	// cluster does across its members.
	if err := a.runStep(ctx, id, aioMongoKeyFileScript, nil, pr.logln); err != nil {
		return fmt.Errorf("create mongo keyFile: %w", err)
	}

	declBy := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if aioFamilyOf(in.Kind) == famMongo {
			declBy[aioSanitizeInst(in.Name)] = in
		}
	}
	members := aioMembersOfFamily(cfg, famMongo)
	prepare := func(only func(aioInstanceRuntime) bool) error {
		for i, m := range members {
			if !fresh[m.Inst] || !only(m) {
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
			pr.phase(fmt.Sprintf("Preparing MongoDB instance %s (%d/%d)", m.Inst, i+1, len(members)), base+span/3)
			if err := a.aioMongoPrepare(ctx, id, in, m, cfg, pr); err != nil {
				return err
			}
		}
		return nil
	}

	// Everything EXCEPT the routers first. A mongos refuses to come up until it
	// can reach its config replica set, so starting it before the config servers
	// exist just fails ("Connection refused" against the configDB) — the routers
	// are started further down, after the config RS has been initiated.
	if err := prepare(func(m aioInstanceRuntime) bool { return m.Role != "mongos" }); err != nil {
		return err
	}

	// Initiate replica sets / assemble the sharded cluster, then create the admin
	// user. Order matters: the localhost exception that lets us create the first
	// user only applies before any user exists, and on a replica set it must run
	// against the PRIMARY.
	for _, in := range n.AIOInstances {
		switch in.Kind {
		case "psmdb":
			if err := a.aioMongoStandaloneAdmin(ctx, id, in, cfg, sec, pr, base+(span*2)/3); err != nil {
				return err
			}
		case "psmrs":
			if err := a.aioMongoInitRS(ctx, id, in, cfg, sec, pr, base+(span*2)/3); err != nil {
				return err
			}
		case "psmdbsharded":
			startRouters := func() error {
				return prepare(func(m aioInstanceRuntime) bool {
					return m.Role == "mongos" && m.Group == aioSanitizeInst(in.Name)
				})
			}
			if err := a.aioMongoInitSharded(ctx, id, in, cfg, sec, pr, base+(span*2)/3, startRouters); err != nil {
				return err
			}
		}
	}
	return nil
}

// aioMongoRSName is the replica-set name a member belongs to. Shard members get
// one RS per shard; the sharded cluster's config servers get their own.
func aioMongoRSName(in aioInstance, m aioInstanceRuntime, cfg aioConfig) string {
	group := aioSanitizeInst(in.Name)
	switch in.Kind {
	case "psmrs":
		return group
	case "psmdbsharded":
		switch m.Role {
		case "config":
			return group + "-cfg"
		case "shard":
			return fmt.Sprintf("%s-shard%d", group, aioMongoShardIndex(m, cfg))
		}
	}
	return ""
}

// aioMongoShardIndex is a shard member's shard number, derived from its position
// among the instance's shard members. Keeping it positional means the planner and
// the initiator agree without storing it.
func aioMongoShardIndex(m aioInstanceRuntime, cfg aioConfig) int {
	i := 0
	for _, x := range cfg.Instances {
		if x.Group != m.Group || x.Role != "shard" {
			continue
		}
		if x.Inst == m.Inst {
			return i
		}
		i++
	}
	return 0
}

// aioMongoPrepare writes one member's config and unit and starts it.
func (a *App) aioMongoPrepare(ctx context.Context, id string, in aioInstance, m aioInstanceRuntime, cfg aioConfig, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}

	isMongos := m.Role == "mongos"
	conf := aioMongodConf(l, m, in, cfg)
	name := "mongod.conf"
	if isMongos {
		name = "mongos.conf"
		l.ConfPath = l.ConfDir + "/mongos.conf"
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, name, 0o644, []byte(conf)); err != nil {
		return fmt.Errorf("%s: write %s: %w", m.Inst, name, err)
	}

	bin := "/usr/bin/mongod"
	if isMongos {
		bin = "/usr/bin/mongos"
	}
	spec := aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One %s instance %s (port %d)", m.Kind, m.Inst, m.Ports.Client),
		ExecStart:   fmt.Sprintf("%s --config %s", bin, l.ConfPath),
		Type:        "simple", // the config sets fork:false, so systemd tracks the process directly
		TimeoutSec:  180,
		EnvFile: fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_DATADIR=%s\nAIO_CNF=%s\n",
			m.Inst, m.Kind, m.Ports.Client, l.DataDir, l.ConfPath),
	}
	if err := a.aioWriteUnit(ctx, id, l, spec); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}
	if err := a.runStep(ctx, id, aioMongoWaitScript, []string{
		fmt.Sprintf("PORT=%d", m.Ports.Client), "LOGERR=" + l.LogErr,
	}, pr.logln); err != nil {
		return fmt.Errorf("%s: did not become reachable: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s running on port %d (%s)", m.Inst, m.Ports.Client, m.Role))
	return nil
}

// aioMongodConf renders one member's mongod.conf (or mongos.conf for a router).
func aioMongodConf(l instLayout, m aioInstanceRuntime, in aioInstance, cfg aioConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — instance %s (%s/%s). Generated.\n", m.Inst, m.Kind, m.Role)

	if m.Role == "mongos" {
		// A router has no dbPath; it points at the config replica set instead.
		fmt.Fprintf(&b, "systemLog:\n  destination: file\n  path: %s\n  logAppend: true\n", l.LogErr)
		fmt.Fprintf(&b, "net:\n  port: %d\n  bindIpAll: true\n", m.Ports.Client)
		fmt.Fprintf(&b, "processManagement:\n  fork: false\n  pidFilePath: %s/mongos.pid\n", l.RunDir)
		fmt.Fprintf(&b, "security:\n  keyFile: %s\n", mongoKeyFile)
		fmt.Fprintf(&b, "sharding:\n  configDB: %s\n", aioMongoConfigDB(m, cfg, in))
		return b.String()
	}

	fmt.Fprintf(&b, "storage:\n  dbPath: %s\n", l.DataDir)
	fmt.Fprintf(&b, "systemLog:\n  destination: file\n  path: %s\n  logAppend: true\n", l.LogErr)
	fmt.Fprintf(&b, "net:\n  port: %d\n  bindIpAll: true\n", m.Ports.Client)
	fmt.Fprintf(&b, "processManagement:\n  fork: false\n  pidFilePath: %s/mongod.pid\n", l.RunDir)
	// A standalone needs no internal cluster auth; everything else does.
	if in.Kind == "psmdb" {
		b.WriteString("security:\n  authorization: enabled\n")
	} else {
		fmt.Fprintf(&b, "security:\n  keyFile: %s\n  authorization: enabled\n", mongoKeyFile)
	}
	if rs := aioMongoRSName(in, m, cfg); rs != "" {
		fmt.Fprintf(&b, "replication:\n  replSetName: %s\n", rs)
	}
	switch m.Role {
	case "config":
		b.WriteString("sharding:\n  clusterRole: configsvr\n")
	case "shard":
		b.WriteString("sharding:\n  clusterRole: shardsvr\n")
	}
	return b.String()
}

// aioMongoConfigDB is the mongos --configdb value: the config replica set and
// every one of its members as host:port. All in this container, so the host is
// constant and the port is what distinguishes them.
func aioMongoConfigDB(mongos aioInstanceRuntime, cfg aioConfig, in aioInstance) string {
	var hosts []string
	for _, x := range cfg.Instances {
		if x.Group == mongos.Group && x.Role == "config" {
			hosts = append(hosts, fmt.Sprintf("127.0.0.1:%d", x.Ports.Client))
		}
	}
	return aioSanitizeInst(in.Name) + "-cfg/" + strings.Join(hosts, ",")
}

// aioMongoRSConfig renders the rs.initiate document for a set of members.
func aioMongoRSConfig(rs string, members []aioInstanceRuntime, configsvr bool) string {
	var parts []string
	for i, m := range members {
		parts = append(parts, fmt.Sprintf(`{_id:%d,host:"127.0.0.1:%d"}`, i, m.Ports.Client))
	}
	cfgsvr := ""
	if configsvr {
		cfgsvr = ",configsvr:true"
	}
	return fmt.Sprintf(`{_id:"%s"%s,members:[%s]}`, rs, cfgsvr, strings.Join(parts, ","))
}

// aioMongoStandaloneAdmin creates the root user on a standalone instance.
func (a *App) aioMongoStandaloneAdmin(ctx context.Context, id string, in aioInstance, cfg aioConfig, sec pxcSecrets, pr *pxcProg, pct int) error {
	inst := aioSanitizeInst(in.Name)
	var m aioInstanceRuntime
	for _, x := range cfg.Instances {
		if x.Inst == inst {
			m = x
		}
	}
	if m.Inst == "" {
		return nil
	}
	pr.phase("Creating MongoDB admin user on "+inst, pct)
	return a.aioMongoCreateAdmin(ctx, id, m.Ports.Client, in, sec, pr)
}

// aioMongoCreateAdmin creates the root admin user via the localhost exception.
func (a *App) aioMongoCreateAdmin(ctx context.Context, id string, port int, in aioInstance, sec pxcSecrets, pr *pxcProg) error {
	pw := strings.TrimSpace(in.RootPassword)
	if pw == "" {
		pw = envOr("MONGO_ADMIN_PASSWORD", envOr("MYSQL_ADMIN_PASSWORD", "admin_password"))
	}
	if err := a.runStep(ctx, id, aioMongoCreateAdminScript, []string{
		fmt.Sprintf("PORT=%d", port), "ADMIN_USER=admin", "ADMIN_PW=" + pw,
	}, pr.logln); err != nil {
		return fmt.Errorf("create admin user on port %d: %w", port, err)
	}
	pr.logln(fmt.Sprintf("admin user created on port %d", port))
	return nil
}

// aioMongoInitRS initiates a plain replica set and creates its admin user.
func (a *App) aioMongoInitRS(ctx context.Context, id string, in aioInstance, cfg aioConfig, sec pxcSecrets, pr *pxcProg, pct int) error {
	group := aioSanitizeInst(in.Name)
	var members []aioInstanceRuntime
	for _, m := range cfg.Instances {
		if m.Group == group && m.Kind == "psmrs" {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return nil
	}
	pr.phase("Initiating replica set "+in.Name, pct)
	if err := a.aioMongoRunInit(ctx, id, group, members, false, pr); err != nil {
		return err
	}
	// The admin user must be created on the PRIMARY, which rs.initiate makes the
	// first member.
	return a.aioMongoCreateAdmin(ctx, id, members[0].Ports.Client, in, sec, pr)
}

// aioMongoRunInit runs rs.initiate on the first member and waits for a PRIMARY.
func (a *App) aioMongoRunInit(ctx context.Context, id, rs string, members []aioInstanceRuntime, configsvr bool, pr *pxcProg) error {
	if err := a.runStep(ctx, id, aioMongoInitRSScript, []string{
		fmt.Sprintf("PORT=%d", members[0].Ports.Client),
		"RS=" + rs,
		"RSCFG=" + aioMongoRSConfig(rs, members, configsvr),
	}, pr.logln); err != nil {
		return fmt.Errorf("initiate replica set %s: %w", rs, err)
	}
	pr.logln(fmt.Sprintf("replica set %s initiated (%d member(s), PRIMARY on 127.0.0.1:%d)",
		rs, len(members), members[0].Ports.Client))
	return nil
}

// aioMongoInitSharded assembles a sharded cluster: initiate the config replica
// set, initiate each shard's replica set, create the admin user through mongos,
// then add every shard.
func (a *App) aioMongoInitSharded(ctx context.Context, id string, in aioInstance, cfg aioConfig, sec pxcSecrets, pr *pxcProg, pct int, startRouters func() error) error {
	group := aioSanitizeInst(in.Name)
	var mongos, config []aioInstanceRuntime
	shards := map[int][]aioInstanceRuntime{}
	for _, m := range cfg.Instances {
		if m.Group != group {
			continue
		}
		switch m.Role {
		case "mongos":
			mongos = append(mongos, m)
		case "config":
			config = append(config, m)
		case "shard":
			i := aioMongoShardIndex(m, cfg)
			shards[i] = append(shards[i], m)
		}
	}
	if len(mongos) == 0 || len(config) == 0 {
		return fmt.Errorf("sharded cluster %s needs a mongos and at least one config server", in.Name)
	}

	pr.phase("Initiating config replica set for "+in.Name, pct)
	if err := a.aioMongoRunInit(ctx, id, group+"-cfg", config, true, pr); err != nil {
		return err
	}
	for i := 0; i < len(shards); i++ {
		if err := a.aioMongoRunInit(ctx, id, fmt.Sprintf("%s-shard%d", group, i), shards[i], false, pr); err != nil {
			return err
		}
	}

	// Only now can the routers start: their configDB is live and initiated.
	if err := startRouters(); err != nil {
		return err
	}

	// The admin user is created through mongos, so it lands in the config RS and
	// every shard inherits it — the same order the classic sharded path uses.
	if err := a.aioMongoCreateAdmin(ctx, id, mongos[0].Ports.Client, in, sec, pr); err != nil {
		return err
	}

	pw := strings.TrimSpace(in.RootPassword)
	if pw == "" {
		pw = envOr("MONGO_ADMIN_PASSWORD", envOr("MYSQL_ADMIN_PASSWORD", "admin_password"))
	}
	var specs []string
	for i := 0; i < len(shards); i++ {
		var hosts []string
		for _, m := range shards[i] {
			hosts = append(hosts, fmt.Sprintf("127.0.0.1:%d", m.Ports.Client))
		}
		specs = append(specs, fmt.Sprintf("%s-shard%d/%s", group, i, strings.Join(hosts, ",")))
	}
	if err := a.runStep(ctx, id, aioMongoAddShardsScript, []string{
		fmt.Sprintf("PORT=%d", mongos[0].Ports.Client),
		"ADMIN_USER=admin", "ADMIN_PW=" + pw,
		"SHARDS=" + strings.Join(specs, " "),
	}, pr.logln); err != nil {
		return fmt.Errorf("add shards to %s: %w", in.Name, err)
	}
	pr.logln(fmt.Sprintf("sharded cluster %s assembled (%d shard(s), mongos on 127.0.0.1:%d)",
		in.Name, len(shards), mongos[0].Ports.Client))
	return nil
}

// ------------------------------------------------------------------ scripts

// aioMongoKeyFileScript creates the shared internal-auth keyFile once. Every
// replica set and shard in this container uses it, as a classic multi-node
// cluster does across its members.
const aioMongoKeyFileScript = `set -e
if [ ! -s ` + mongoKeyFile + ` ]; then
  openssl rand -base64 756 > ` + mongoKeyFile + `
fi
id mongod >/dev/null 2>&1 || useradd -r -s /sbin/nologin mongod 2>/dev/null || true
chown mongod:mongod ` + mongoKeyFile + `
chmod 400 ` + mongoKeyFile + `
exit 0`

// aioMongoWaitScript waits until an instance answers a ping on its own port.
const aioMongoWaitScript = `set -e
OK=0
for i in $(seq 1 60); do
  mongosh --quiet --port "$PORT" --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1 && { OK=1; break; }
  sleep 2
done
if [ "$OK" != 1 ]; then
  echo "mongod/mongos on port $PORT did not become reachable:"
  tail -20 "$LOGERR" 2>/dev/null
  exit 1
fi
exit 0`

// aioMongoInitRSScript runs rs.initiate on $PORT and waits for a PRIMARY.
// Idempotent: a re-run finds the set already initiated.
const aioMongoInitRSScript = `set -e
mongosh --quiet --port "$PORT" --eval 'try { rs.initiate('"$RSCFG"') } catch (e) { if (!/already initialized/i.test(e.message)) throw e }'
OK=0
for i in $(seq 1 60); do
  S=$(mongosh --quiet --port "$PORT" --eval 'try{rs.status().myState}catch(e){-1}' 2>/dev/null)
  [ "$S" = "1" ] && { OK=1; break; }
  sleep 2
done
if [ "$OK" != 1 ]; then
  echo "replica set $RS has no PRIMARY:"
  mongosh --quiet --port "$PORT" --eval 'try{rs.status()}catch(e){print(e)}' 2>/dev/null | head -30
  exit 1
fi
exit 0`

// aioMongoCreateAdminScript creates the root admin user via the localhost
// exception, then proves it can authenticate. Idempotent.
const aioMongoCreateAdminScript = `set -e
mongosh --quiet --port "$PORT" --eval 'db.getSiblingDB("admin").createUser({user:"'"$ADMIN_USER"'",pwd:"'"$ADMIN_PW"'",roles:[{role:"root",db:"admin"}]})' 2>&1 | grep -viE 'already exists' || true
mongosh --quiet --port "$PORT" -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval 'db.adminCommand({ping:1})' >/dev/null
exit 0`

// aioMongoAddShardsScript registers every shard with the cluster through mongos.
const aioMongoAddShardsScript = `set -e
for s in $SHARDS; do
  mongosh --quiet --port "$PORT" -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin \
    --eval 'sh.addShard("'"$s"'")' 2>&1 | grep -viE 'already a member|already exists' || true
done
mongosh --quiet --port "$PORT" -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin \
  --eval 'sh.status()' >/dev/null
exit 0`
