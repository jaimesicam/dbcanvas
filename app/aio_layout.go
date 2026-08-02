package main

import (
	"fmt"
	"strings"
)

// aio_layout.go — where one All-in-One instance's files live and what its
// service is called.
//
// Every classic node type owns its container, so it can hardcode /var/lib/mysql,
// /etc/my.cnf and the vendor unit name. An All-in-One container has N of them, so
// each instance gets a private tree under /opt/aio/<inst> and a dbcanvas-authored
// unit aio-<inst>.service. Vendor units are masked at provision time so nothing
// can grab a default port behind our back.
//
// Ownership deliberately stays with the vendor OS user (mysql, postgres, mongod,
// valkey, proxysql, haproxy): only the directories move, so SELinux labels,
// package post-install scripts and the binaries themselves behave as they do on
// a single-product node.

const (
	aioRoot     = "/opt/aio"          // per-instance trees
	aioEtc      = "/etc/dbcanvas/aio" // registry + per-instance env files
	aioRunRoot  = "/run/aio"          // sockets/pids (tmpfs, recreated by RuntimeDirectory=)
	aioCtlPath  = "/usr/local/bin/aioctl"
	aioTarget   = "aio.target"
	aioRegistry = aioEtc + "/instances.tsv"
)

// instLayout is one instance's filesystem + service identity. A zero Inst means
// "the classic single-product layout" — see defaultLayout.
type instLayout struct {
	Inst  string // instance id, e.g. "ps01" or "pxc-cluster-01-n2" ("" = classic node)
	Kind  string // aioKind.Kind
	Owner string // OS user:group that must own the tree

	Unit     string // systemd unit name, without ".service"
	Dir      string // instance root
	ConfDir  string
	ConfPath string
	DataDir  string
	LogDir   string
	LogErr   string
	RunDir   string
	Sock     string
	EnvFile  string

	Ports aioPorts
}

// aioInstDir etc. are the canonical paths for an instance id.
func aioInstDir(inst string) string { return aioRoot + "/" + inst }

// aioLayout builds the layout for an All-in-One instance. Product-specific file
// names (my.cnf vs postgresql.conf vs mongod.conf) are chosen from the family so
// the on-disk tree reads the way an operator expects.
func aioLayout(inst, kind string, ports aioPorts) instLayout {
	dir := aioInstDir(inst)
	l := instLayout{
		Inst:    inst,
		Kind:    kind,
		Unit:    "aio-" + inst,
		Dir:     dir,
		ConfDir: dir + "/etc",
		DataDir: dir + "/data",
		LogDir:  dir + "/log",
		RunDir:  aioRunRoot + "/" + inst,
		EnvFile: aioEtc + "/" + inst + ".env",
		Ports:   ports,
	}
	switch aioFamilyOf(kind) {
	case famMySQL:
		l.Owner = "mysql:mysql"
		l.ConfPath = l.ConfDir + "/my.cnf"
		l.LogErr = l.LogDir + "/error.log"
		l.Sock = l.RunDir + "/mysql.sock"
	case famPG:
		l.Owner = "postgres:postgres"
		l.ConfPath = l.DataDir + "/postgresql.conf" // initdb writes it into PGDATA
		l.LogErr = l.LogDir + "/postgresql.log"
		l.Sock = l.RunDir
	case famMongo:
		l.Owner = "mongod:mongod"
		l.ConfPath = l.ConfDir + "/mongod.conf"
		l.LogErr = l.LogDir + "/mongod.log"
		l.Sock = l.RunDir + "/mongodb.sock"
	case famValkey:
		l.Owner = "valkey:valkey"
		l.ConfPath = l.ConfDir + "/valkey.conf"
		l.LogErr = l.LogDir + "/valkey.log"
		l.Sock = l.RunDir + "/valkey.sock"
	case famProxy:
		l.Owner = "proxysql:proxysql"
		l.ConfPath = l.ConfDir + "/proxysql.cnf"
		l.LogErr = l.LogDir + "/proxysql.log"
		l.Sock = l.RunDir + "/proxysql_admin.sock"
	case famHAProxy:
		l.Owner = "haproxy:haproxy"
		l.ConfPath = l.ConfDir + "/haproxy.cfg"
		l.LogErr = l.LogDir + "/haproxy.log"
	case famOrch:
		l.Owner = "root:root"
		l.ConfPath = l.ConfDir + "/orchestrator.conf.json"
		l.LogErr = l.LogDir + "/orchestrator.log"
	default:
		l.Owner = "root:root"
		l.ConfPath = l.ConfDir + "/config"
		l.LogErr = l.LogDir + "/service.log"
	}
	return l
}

// defaultLayout returns the layout a classic single-product node already uses —
// the exact paths, unit names and ports hardcoded in mysql.go/pg.go/mongodb.go
// today. Existing call sites pass this, so threading instLayout through shared
// helpers changes nothing for them.
func defaultLayout(kind, os string) instLayout {
	l := instLayout{Kind: kind}
	switch aioFamilyOf(kind) {
	case famMySQL:
		l.Owner = "mysql:mysql"
		l.Unit = mysqlUnit(os)
		l.ConfDir, _ = pxcCnfDir(os)
		l.ConfPath = pxcCnfPath(os)
		l.DataDir = "/var/lib/mysql"
		l.LogErr = pxcLogError(os)
		l.RunDir = "/var/run/mysqld"
		l.Sock = "/var/lib/mysql/mysql.sock"
		l.Ports = aioPorts{Base: 3306, Client: 3306, Admin: 33060, Group: 4567, IST: 4568, SST: 4444, Check: 9200}
	case famPG:
		l.Owner = "postgres:postgres"
		l.Unit = "postgresql"
		l.DataDir = "/var/lib/pgsql/data"
		l.RunDir = "/var/run/postgresql"
		l.Ports = aioPorts{Base: 5432, Client: 5432, REST: 8008, EtcdCli: 2379, EtcdPr: 2380}
	case famMongo:
		l.Owner = "mongod:mongod"
		l.Unit = "mongod"
		l.ConfPath = "/etc/mongod.conf"
		l.DataDir = "/var/lib/mongo"
		l.LogDir = "/var/log/mongo"
		l.RunDir = "/var/run/mongodb"
		l.Ports = aioPorts{Base: 27017, Client: 27017}
	case famValkey:
		l.Owner = "valkey:valkey"
		l.Unit = "valkey"
		l.DataDir = valkeyDataDir
		l.Ports = aioPorts{Base: 6379, Client: 6379, Group: 16379}
	case famProxy:
		l.Owner = "proxysql:proxysql"
		l.Unit = "proxysql"
		l.ConfPath = "/etc/proxysql.cnf"
		l.DataDir = "/var/lib/proxysql"
		l.Ports = aioPorts{Base: 6033, Client: 6033, Admin: 6032, Group: 6035}
	case famHAProxy:
		l.Owner = "haproxy:haproxy"
		l.Unit = "haproxy"
		l.ConfPath = "/etc/haproxy/haproxy.cfg"
		l.Ports = aioPorts{Base: 5000, Client: 5000, Check: 5001, Admin: 8404}
	case famOrch:
		l.Owner = "root:root"
		l.Unit = orchestratorUnit
		l.ConfPath = "/etc/orchestrator.conf.json"
		l.Ports = aioPorts{Base: orchestratorPort, Client: orchestratorPort}
	}
	return l
}

// aioSanitizeInst makes an instance id safe as a filename, a systemd unit name
// and a DNS label all at once: lowercase [a-z0-9-], no leading/trailing dash.
func aioSanitizeInst(s string) string { return hostLabel(s) }

// aioMemberInst is the instance id of member i of a cluster instance. A
// single-member instance keeps the bare name so "ps01" does not become
// "ps01-n1"; clusters get -n1, -n2, … in provisioning order.
func aioMemberInst(name string, kind string, member, total int) string {
	base := aioSanitizeInst(name)
	if total <= 1 && !aioKindByID[kind].Cluster {
		return base
	}
	return fmt.Sprintf("%s-n%d", base, member+1)
}

// aioMkdirScript creates one instance's tree with the right ownership. Runs
// after the product's packages are installed, since it needs the vendor user to
// exist.
func (l instLayout) mkdirScript() string {
	user, group := l.userGroup()
	var b strings.Builder
	b.WriteString("set -e\n")
	// The vendor user normally exists from the package post-install; create it
	// defensively so a mongos-only or proxy-only container still works.
	if user != "root" {
		fmt.Fprintf(&b, "id %s >/dev/null 2>&1 || useradd -r -s /sbin/nologin %s 2>/dev/null || true\n", user, user)
	}
	for _, d := range []string{l.Dir, l.ConfDir, l.DataDir, l.LogDir, l.RunDir} {
		if d == "" {
			continue
		}
		fmt.Fprintf(&b, "install -d -m 0750 -o %s -g %s %s\n", user, group, d)
	}
	return b.String()
}

// userGroup splits Owner ("mysql:mysql") into its user and group halves.
func (l instLayout) userGroup() (string, string) {
	user, group, ok := strings.Cut(l.Owner, ":")
	if !ok || group == "" {
		group = user
	}
	if user == "" {
		return "root", "root"
	}
	return user, group
}
