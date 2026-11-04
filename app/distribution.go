package main

import (
	"strings"
)

// distribution.go — whose build a MongoDB or PostgreSQL node installs.
//
// The MongoDB and PostgreSQL node types (psm/psmrs/psmdb, pg/patroni/pgstream) were
// written against Percona's packages. The community builds are the same servers: the
// same mongod and postgres, the same config files, the same replication, the same
// on-disk layout. So rather than six new node types — which would each need adding to
// every place that asks "is this a MongoDB node?" (PMM, PBM, the Log Summary, FTDC, the
// Data Generator, the Explorer, packet captures, switchover, …) — a node keeps its type
// and carries a Distribution. Only two things branch on it:
//
//   - packaging: which repository and which package names (this file);
//   - the options a community build does not have, which validation refuses and the
//     design form does not offer (distributionIssues).
//
// MySQL Community took the other road (its own node types, mysqlce.go) because it
// was added as its own family with its own frames; the reuse there is at the level of
// the provisioning steps. Here the whole node is reused.
//
// The version fields stay psmdbMajor/psmdbVersion and pgMajor/pgVersion. Both upstreams
// number their releases the way Percona does (Percona tracks them: PSMDB 8.0.12-4 is
// MongoDB 8.0.12, PPG 17.5 is PostgreSQL 17.5), and every version gate reads those
// fields, so a community node passes through them unchanged. The distribution is fixed
// when the node is added, so a version is never reinterpreted under another one.

const (
	distPercona = ""          // Percona's build: PSMDB, Percona Distribution for PostgreSQL
	distMongoDB = "community" // MongoDB Community Server, repo.mongodb.org
	distPGDG    = "pgdg"      // PostgreSQL Global Development Group, download/apt.postgresql.org
)

// mongoTypes / pgTypes are the node and frame types a distribution applies to.
func distMongoType(t string) bool { return t == "psm" || t == "psmrs" || t == "psmdb" }
func distPGType(t string) bool    { return t == "pg" || t == "patroni" || t == "pgstream" }

// isMongoCommunity / isPGDG read a node's or frame's distribution.
func isMongoCommunity(dist string) bool { return strings.TrimSpace(dist) == distMongoDB }
func isPGDG(dist string) bool           { return strings.TrimSpace(dist) == distPGDG }

// distributionLabel names a type under its distribution the way the library does:
// MDB / MDB Replica Set / MDB Sharded and PostgreSQL / Patroni Cluster / PostgreSQL
// Replication with "(PGDG)" after them. Used by validation messages.
func distributionLabel(typ, dist string) string {
	if isMongoCommunity(dist) {
		switch typ {
		case "psm":
			return "MDB"
		case "psmrs":
			return "MDB Replica Set"
		case "psmdb":
			return "MDB Sharded"
		}
	}
	if isPGDG(dist) {
		switch typ {
		case "pg":
			return "PostgreSQL (PGDG)"
		case "patroni":
			return "Patroni Cluster (PGDG)"
		case "pgstream":
			return "PostgreSQL Replication (PGDG)"
		}
	}
	return typ
}

// ------------------------------------------------------------------ MongoDB Community

// mdbMajors are the Community Server series offered. 6.0 is past end of life but
// still published (and still what some customers run); 8.2 is the current release
// after 8.0. The rapid releases before it (7.1-7.3, 8.1) are left out for the reason
// MySQL's innovation releases are: superseded the quarter they ship.
var mdbMajors = []string{"6.0", "7.0", "8.0", "8.2"}

// mdbMajorOf normalizes a Community Server series, defaulting to 8.0.
func mdbMajorOf(major string) string {
	major = strings.TrimSpace(major)
	for _, m := range mdbMajors {
		if major == m || major == strings.TrimSuffix(m, ".0") {
			return m
		}
	}
	return "8.0"
}

// mdbKeyMajor is the signing key a series' repository is checked against. MongoDB
// publishes one key per x.0 series and signs the minor releases after it with that
// key: there is no server-8.2.asc, 8.2 is signed with the 8.0 key.
func mdbKeyMajor(major string) string {
	major = mdbMajorOf(major)
	if i := strings.IndexByte(major, '.'); i > 0 {
		return major[:i] + ".0"
	}
	return major
}

// mdbPackages is what a community member installs: the server and mongos, pinned to
// the node's version, then the shell and the database tools, which are versioned on
// their own (mongosh 2.x, tools 100.x) and so are installed unpinned. Both binaries go
// on every member, as the Percona path's AiO install does — mongos is small, and a
// member can then take either role.
func mdbPackages(role string) (pinned, tools string) {
	pinned = "mongodb-org-server"
	if role == "mongos" {
		pinned = "mongodb-org-mongos"
	}
	return pinned, "mongodb-mongosh mongodb-database-tools"
}

// mdbInstallRHEL / mdbInstallDebian add repo.mongodb.org for one series and install
// from it. Env: MAJOR, KEY (mdbKeyMajor), PKGS (pinned to VER), TOOLS (unpinned).
//
// The community package runs mongod as `mongod` on EL but `mongodb` on Debian, while
// everything downstream (mongoStartMongodScript, the keyFile, the TLS files, PBM)
// chowns to mongod — the name Percona uses on both. So a drop-in runs the service as
// mongod everywhere, and the user is created where the package did not.
const mdbInstallRHEL = pinInstallRHEL + `set -e
el=$(rpm -E %rhel)
arch=$(uname -m)
cat > /etc/yum.repos.d/mongodb-org-$MAJOR.repo <<REPO
[mongodb-org-$MAJOR]
name=MongoDB Community Server $MAJOR
baseurl=https://repo.mongodb.org/yum/redhat/$el/mongodb-org/$MAJOR/$arch/
gpgcheck=1
enabled=1
gpgkey=https://pgp.mongodb.com/server-$KEY.asc
REPO
pin_install $PKGS
VER= pin_install $TOOLS
` + mdbRunAsMongod

const mdbInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq curl ca-certificates gnupg >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq curl ca-certificates gnupg >/dev/null; }
. /etc/os-release
case "$ID" in
  ubuntu) base=https://repo.mongodb.org/apt/ubuntu; comp=multiverse ;;
  *)      base=https://repo.mongodb.org/apt/debian; comp=main ;;
esac
curl -fsSL "https://pgp.mongodb.com/server-$KEY.asc" | gpg --batch --yes --dearmor -o /usr/share/keyrings/mongodb-server-$KEY.gpg
echo "deb [signed-by=/usr/share/keyrings/mongodb-server-$KEY.gpg] $base ${VERSION_CODENAME}/mongodb-org/$MAJOR $comp" > /etc/apt/sources.list.d/mongodb-org-$MAJOR.list
apt-get update -qq >/dev/null
pin_install $PKGS
VER= pin_install $TOOLS
` + mdbRunAsMongod

const mdbRunAsMongod = `id mongod >/dev/null 2>&1 || useradd -r -U -M -d /var/lib/mongo -s /usr/sbin/nologin mongod
mkdir -p /etc/systemd/system/mongod.service.d
cat > /etc/systemd/system/mongod.service.d/05-dbcanvas-user.conf <<'DROPIN'
[Service]
User=mongod
Group=mongod
DROPIN
systemctl daemon-reload 2>/dev/null || true`

// mongoInstallStep is the install a MongoDB member runs for its distribution: the
// script and its environment, plus what the deploy log calls it.
func mongoInstallStep(dist, os, major, version, role string) (script string, env []string, what string) {
	if isMongoCommunity(dist) {
		pinned, tools := mdbPackages(role)
		script = mdbInstallRHEL
		if isDebianOS(os) {
			script = mdbInstallDebian
		}
		m := mdbMajorOf(major)
		return script, []string{"MAJOR=" + m, "KEY=" + mdbKeyMajor(m), "PKGS=" + pinned, "TOOLS=" + tools, "VER=" + version}, "MongoDB Community Server " + m
	}
	pkgs := "percona-server-mongodb-server percona-server-mongodb-tools percona-mongodb-mongosh"
	if role == "mongos" {
		pkgs = "percona-server-mongodb-mongos percona-server-mongodb-tools percona-mongodb-mongosh"
	}
	script = mongoInstallRHEL
	if isDebianOS(os) {
		script = mongoInstallDebian
	}
	return script, []string{"PSMDB_REPO=" + psmdbRepo(major), "PKGS=" + pkgs, "VER=" + version}, "Percona Server for MongoDB"
}

// replaceEnv sets KEY=value in an environment list, replacing an existing entry.
func replaceEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}

// ------------------------------------------------------------------ PGDG

// pgdgServerPackages is PGDG's server for one major. The names are PGDG's own, which
// Percona's mirror with a percona- prefix (percona-postgresql17-server ↔
// postgresql17-server), and the layout is the same: /usr/pgsql-NN on EL, the
// postgresql@NN-main cluster on Debian — so pgBinDir, pgDataDir, pgConfDir and
// pgServiceName hold for both.
func pgdgServerPackages(os, major string) []string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return []string{"postgresql-" + major}
	}
	return []string{"postgresql" + major + "-server", "postgresql" + major + "-contrib"}
}

// pgdgPatroniPackages is Patroni and etcd for a PGDG cluster. On EL both come from
// PGDG — Patroni from pgdg-common, etcd from the pgdg-rhelN-extras repository, which
// is disabled by default and enabled for this install (EXTRAS).
//
// PGDG's apt repository has Patroni but no etcd, and the distributions' own etcd will
// not do: Debian and Ubuntu build etcd without its gRPC gateway (every /v3/... HTTP
// path answers 404), and the gateway is exactly what Patroni's etcd3 client talks to —
// Patroni 4.1 against Ubuntu 24.04's etcd-server 3.4.30 loops on "Failed to get list of
// machines" forever. So on Debian and Ubuntu etcd is the etcd project's own release
// (pgdgEtcdUpstreamScript), the build every other Patroni here runs.
func pgdgPatroniPackages(os string) []string {
	if isDebianOS(os) {
		return []string{"patroni"}
	}
	return []string{"patroni", "patroni-etcd", "etcd"}
}

// pgdgEtcdVersion is the etcd release a PGDG Patroni cluster on Debian or Ubuntu runs:
// the 3.5 line, which is what Percona ships for its Patroni and what PGDG's EL
// extras repository carried until 3.7.
const pgdgEtcdVersion = "3.5.35"

// pgdgEtcdUpstreamScript installs etcd and etcdctl from the etcd project's release
// tarball (as kubeadmEtcdctlScript does for etcdctl) and a unit that starts etcd from
// /etc/etcd/etcd.conf.yaml as the etcd user — the unit Percona's package provides.
// Env: VER, and HTTPS_PROXY when the node downloads through the Intranet's proxy.
const pgdgEtcdUpstreamScript = `set -e
export https_proxy="${HTTPS_PROXY:-}"
A="$(dpkg --print-architecture)"
if [ "$(/usr/local/bin/etcd --version 2>/dev/null | sed -n 's/^etcd Version: //p')" != "$VER" ]; then
  D="$(mktemp -d)"
  curl -fsSL --retry 3 "https://github.com/etcd-io/etcd/releases/download/v$VER/etcd-v$VER-linux-$A.tar.gz" | tar xz -C "$D"
  install -m 0755 "$D/etcd-v$VER-linux-$A/etcd" "$D/etcd-v$VER-linux-$A/etcdctl" "$D/etcd-v$VER-linux-$A/etcdutl" /usr/local/bin/
  rm -rf "$D"
fi
id etcd >/dev/null 2>&1 || useradd -r -U -M -d /var/lib/etcd -s /usr/sbin/nologin etcd
install -d -o etcd -g etcd /var/lib/etcd
mkdir -p /etc/etcd
cat > /etc/systemd/system/etcd.service <<'UNIT'
[Unit]
Description=etcd (etcd-io release, for Patroni)
Documentation=https://etcd.io/docs
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=etcd
ExecStart=/usr/local/bin/etcd --config-file /etc/etcd/etcd.conf.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
echo "etcd $VER"`

// pgdgExtrasRHEL turns on PGDG's extras repository (where etcd lives on EL) when EXTRAS
// is set. It is spliced into repmgr's EL install (repmgr.go) right before the install,
// after the repository file exists; the repository stays enabled, so the node's later
// package operations see the same set the install did.
const pgdgExtrasRHEL = `if [ -n "$EXTRAS" ]; then
  sed -i '/^\[pgdg-rhel[0-9]*-extras\]/,/^\[/ s/^enabled=0/enabled=1/' /etc/yum.repos.d/pgdg-redhat-all.repo
fi
`

// pgInstallStep is the install a PostgreSQL member runs for its distribution, for the
// packages given: the script and environment. Percona's goes through percona-release
// (ppg-NN); PGDG's adds the PGDG repository the way repmgr's install does.
func pgInstallStep(dist, os, osVersion, major, version string, pkgs []string, extras bool) (script string, env []string) {
	debian := isDebianOS(os)
	if isPGDG(dist) {
		env = []string{"PKGS=" + strings.Join(pkgs, " "), "VER=" + version}
		if debian {
			return repmgrInstallDebian, env
		}
		if extras {
			env = append(env, "EXTRAS=1")
		}
		// EPEL carries dependencies of PGDG's packages on Oracle Linux (pgbackrest's
		// libssh2, patroni's Python modules on EL8), as it does for repmgr.
		env = append(env, "WITH_EPEL=1", "EPELPKG="+epelPackage(osVersion))
		return strings.Replace(repmgrInstallRHEL, "pin_install $PKGS", pgdgExtrasRHEL+"pin_install $PKGS", 1), env
	}
	env = []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs, " "), "VER=" + version}
	if debian {
		return patroniInstallDebian, env
	}
	return patroniInstallRHEL, env
}

// pgServerPackagesFor / pgBackRestPackage are the server and pgBackRest packages under
// a distribution.
func pgServerPackagesFor(dist, os, major string) []string {
	if isPGDG(dist) {
		return pgdgServerPackages(os, major)
	}
	return pgServerPackages(os, major)
}

func pgBackRestPackage(dist string) string {
	if isPGDG(dist) {
		return "pgbackrest"
	}
	return "percona-pgbackrest"
}

// pgDistName is what the deploy log calls the install.
func pgDistName(dist string) string {
	if isPGDG(dist) {
		return "PostgreSQL (PGDG)"
	}
	return "PostgreSQL"
}

// pgVectorFlavor is installPGVector's flavor for a distribution.
func pgVectorFlavor(dist string) string {
	if isPGDG(dist) {
		return "pgdg"
	}
	return "ppg"
}

// ------------------------------------------------------------------ validation

// distributionIssues checks a node's or frame's distribution and refuses what that
// build does not have. who names the node in messages; the option arguments are the
// design's own fields, passed in so one function serves nodes and frames.
type distOptions struct {
	// MongoDB: PSMDB features with no Community Server equivalent. OIDC, LDAP and
	// Kerberos authentication and encryption at rest are MongoDB Enterprise features
	// that PSMDB ships in its free build; the Community Server has none of them.
	OIDC, LDAP, Kerberos, Vault bool
	// Vector search runs Percona's mongot (Percona Search for MongoDB, ps4m repository),
	// which needs PSMDB 8.3 (mongosearch.go).
	VectorSearch bool
	// PostgreSQL: Percona components. pg_tde is Percona's (it needs Percona's patched
	// server); pg_stat_monitor, pgAudit and set_user are offered from Percona's
	// repository here (ppgcomponents.go), with pg_stat_statements the PGDG choice for
	// query analytics, as for repmgr.
	PGTDE, PGStatMonitor, PGAudit, PGSetUser bool
}

func distributionIssues(who, typ, dist string, o distOptions) []issue {
	dist = strings.TrimSpace(dist)
	var out []issue
	switch dist {
	case distPercona:
		return nil
	case distMongoDB:
		if !distMongoType(typ) {
			return []issue{{Level: "error", Message: who + ": distribution \"community\" applies to MongoDB nodes only"}}
		}
		for _, f := range []struct {
			on   bool
			what string
		}{
			{o.OIDC, "OIDC authentication"},
			{o.LDAP, "LDAP authentication"},
			{o.Kerberos, "Kerberos authentication"},
			{o.Vault, "encryption at rest"},
			{o.VectorSearch, "vector search (Percona's mongot)"},
		} {
			if f.on {
				out = append(out, issue{Level: "error", Message: who + ": " + f.what + " is a Percona Server for MongoDB feature (Enterprise-only upstream) — use PSMDB, or turn it off"})
			}
		}
	case distPGDG:
		if !distPGType(typ) {
			return []issue{{Level: "error", Message: who + ": distribution \"pgdg\" applies to PostgreSQL, Patroni and PostgreSQL Replication only"}}
		}
		for _, f := range []struct {
			on   bool
			what string
		}{
			{o.PGTDE, "pg_tde (Transparent Data Encryption) needs Percona's server"},
			{o.PGStatMonitor, "pg_stat_monitor is a Percona package — use pg_stat_statements"},
			{o.PGAudit, "pgAudit is installed from Percona's repository here"},
			{o.PGSetUser, "set_user is installed from Percona's repository here"},
			{o.OIDC, "OIDC login uses Percona's pg_oidc_validator"},
		} {
			if f.on {
				out = append(out, issue{Level: "error", Message: who + " runs PGDG PostgreSQL: " + f.what})
			}
		}
	default:
		out = append(out, issue{Level: "error", Message: who + ": unknown distribution \"" + dist + "\" (\"\" for Percona, \"community\" for MongoDB, \"pgdg\" for PostgreSQL)"})
	}
	return out
}

// pgdgEtcdUnitScript makes the packaged etcd unit start from /etc/etcd/etcd.conf.yaml
// (etcdConfPath), which is how Percona's unit starts and what patroniEtcdConf writes.
// With --config-file etcd ignores its ETCD_* environment, so the variables both
// packages set (Debian's ETCD_NAME=%H and ETCD_DATA_DIR) cannot fight the file.
const pgdgEtcdUnitScript = `set -e
mkdir -p /etc/systemd/system/etcd.service.d
cat > /etc/systemd/system/etcd.service.d/10-dbcanvas-config.conf <<'DROPIN'
[Service]
ExecStart=
ExecStart=/usr/bin/etcd --config-file /etc/etcd/etcd.conf.yaml
DROPIN
systemctl daemon-reload`

// ------------------------------------------------------------------ catalogue

// The per-image version catalogue (versions.yaml) records each distribution in its own
// section: mongodb_community from repo.mongodb.org and pgdg_postgresql from PGDG, beside
// Percona's percona_server_mongodb and percona_postgresql. images/versions.sh probes them
// in its "upstream" group.
const (
	catMongoCommunity = "mongodb_community"
	catPGDG           = "pgdg_postgresql"
)

func mongoCatalogSection(dist string) string {
	if isMongoCommunity(dist) {
		return catMongoCommunity
	}
	return "percona_server_mongodb"
}

func mongoCatalogProduct(dist string) string {
	if isMongoCommunity(dist) {
		return "MongoDB Community"
	}
	return "PS MongoDB"
}

// mongoMajorFor is the series a MongoDB member installs: PSMDB's default is 8.0 and so
// is the Community Server's, but only the community one knows 8.2.
func mongoMajorFor(dist, major string) string {
	if isMongoCommunity(dist) {
		return mdbMajorOf(major)
	}
	return majorOr(major, "8.0")
}

func pgCatalogSection(dist string) string {
	if isPGDG(dist) {
		return catPGDG
	}
	return "percona_postgresql"
}

func pgCatalogProduct(dist string) string {
	if isPGDG(dist) {
		return "PGDG PostgreSQL"
	}
	return "PostgreSQL"
}

func loadMDBCatalog() []PXCImage  { return loadImageCatalog(catMongoCommunity) }
func loadPGDGCatalog() []PXCImage { return loadImageCatalog(catPGDG) }

// distVersionIssues refuses a pinned minor the distribution's catalogue does not list
// for the image. The empty-series case is catalogVersionIssues' (versiongate.go); this
// is the one past it, the same check upstreamVersionIssues makes for MariaDB and MySQL
// Community, and for the same reason: a PSMDB-style "8.0.12-4" or a version from the
// other distribution's catalogue would otherwise fail minutes in, inside dnf or apt.
func distVersionIssues(label, typ, dist, os, osVersion, arch, major, version string) []issue {
	version = strings.TrimSpace(version)
	if version == "" || (!isMongoCommunity(dist) && !isPGDG(dist)) {
		return nil
	}
	var images []PXCImage
	var product string
	if isMongoCommunity(dist) {
		images, product, major = loadMDBCatalog(), "MongoDB Community", mdbMajorOf(major)
	} else {
		images, product, major = loadPGDGCatalog(), "PGDG PostgreSQL", ppgMajorOf(major)
	}
	arch = archOr(arch)
	for _, im := range images {
		if im.OS != os || im.OSVersion != osVersion || im.Arch != arch {
			continue
		}
		avail := im.Versions[major]
		for _, v := range avail {
			if v == version {
				return nil
			}
		}
		if len(avail) == 0 {
			return nil // catalogVersionIssues reports the empty series
		}
		return []issue{{Level: "error", Message: label + ": " + product + " " + version + " is not available for " + os + " " + osVersion + " — newest " + major + " is " + avail[0]}}
	}
	return nil
}

// distributionStackIssues runs the distribution checks over a design: every MongoDB and
// PostgreSQL node and frame, its distribution's options and its pinned version.
func distributionStackIssues(doc designDoc) []issue {
	var out []issue
	for _, n := range doc.Nodes {
		if n.FrameID != "" || n.Distribution == "" {
			continue
		}
		who := "Node " + n.Label
		switch {
		case distMongoType(n.Type):
			out = append(out, distributionIssues(who, n.Type, n.Distribution, distOptions{
				OIDC: n.EnableOIDC, LDAP: n.LdapAuth, Kerberos: n.KerberosAuth, Vault: n.EnableVault})...)
			out = append(out, distVersionIssues(who, n.Type, n.Distribution, n.OS, n.OSVersion, n.Arch, n.PSMDBMajor, n.PSMDBVersion)...)
		case distPGType(n.Type):
			out = append(out, distributionIssues(who, n.Type, n.Distribution, distOptions{
				OIDC: n.EnableOIDC, PGTDE: n.EnableVault, PGStatMonitor: pgQuerySourceOf(n.PGQuerySource) == pgQSMonitor,
				PGAudit: n.PGAudit, PGSetUser: n.PGSetUser})...)
			out = append(out, distVersionIssues(who, n.Type, n.Distribution, n.OS, n.OSVersion, n.Arch, n.PGMajor, n.PGVersion)...)
		default:
			out = append(out, distributionIssues(who, n.Type, n.Distribution, distOptions{})...)
		}
	}
	for _, f := range doc.Frames {
		if f.Distribution == "" {
			continue
		}
		who := "Cluster " + f.Label
		switch {
		case distMongoType(f.Type):
			out = append(out, distributionIssues(who, f.Type, f.Distribution, distOptions{VectorSearch: f.VectorSearch})...)
			out = append(out, distVersionIssues(who, f.Type, f.Distribution, f.OS, f.OSVersion, f.Arch, f.PSMDBMajor, f.PSMDBVersion)...)
		case distPGType(f.Type):
			out = append(out, distributionIssues(who, f.Type, f.Distribution, distOptions{
				PGTDE: f.EnableVault, PGStatMonitor: pgQuerySourceOf(f.PGQuerySource) == pgQSMonitor,
				PGAudit: f.PGAudit, PGSetUser: f.PGSetUser})...)
			out = append(out, distVersionIssues(who, f.Type, f.Distribution, f.OS, f.OSVersion, f.Arch, f.PGMajor, f.PGVersion)...)
		default:
			out = append(out, distributionIssues(who, f.Type, f.Distribution, distOptions{})...)
		}
	}
	return out
}
