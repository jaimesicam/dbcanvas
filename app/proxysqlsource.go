package main

import (
	"fmt"
	"strings"
)

// proxysqlsource.go — where a ProxySQL node's (or ProxySQL cluster's) package comes from, and
// how a PXC backend is then configured.
//
//	""                Percona's proxysql2/proxysql3 from the percona-release proxysql repository,
//	                  configured for PXC with the proxysql-admin it bundles. The default.
//	"upstream-admin"  ProxySQL's own package from repo.proxysql.com, plus proxysql-admin from a
//	                  release tag of percona/proxysql-admin-tool (the same scripts Percona
//	                  bundles; they talk to the ordinary admin interface and check no build).
//	                  PXC is then configured exactly as with Percona's package.
//	"upstream-native" ProxySQL's own package, configured for PXC over the admin interface with
//	                  ProxySQL's native Galera support (mysql_galera_hostgroups), the way the
//	                  ProxySQL project documents it — no proxysql-admin.
//
// A MySQL replication backend is configured over the admin interface for every source
// (proxysqlMySQLConfigureScript). Upstream's package installs the same /usr/bin/proxysql,
// proxysql.service and /etc/proxysql.cnf, so starting, clustering and PMM do not branch on
// the source. Its versions are the upstream_proxysql section of versions.yaml, keyed "2" and
// "3" like Percona's, as plain releases ("3.0.12").

const (
	proxysqlSrcPercona        = ""
	proxysqlSrcUpstreamAdmin  = "upstream-admin"
	proxysqlSrcUpstreamNative = "upstream-native"
)

// proxysqlIsUpstream reports whether a source installs ProxySQL's own package.
func proxysqlIsUpstream(src string) bool {
	s := strings.TrimSpace(src)
	return s == proxysqlSrcUpstreamAdmin || s == proxysqlSrcUpstreamNative
}

func proxysqlSourceLabel(src string) string {
	switch strings.TrimSpace(src) {
	case proxysqlSrcUpstreamAdmin:
		return "upstream ProxySQL with proxysql-admin"
	case proxysqlSrcUpstreamNative:
		return "upstream ProxySQL, native Galera configuration"
	}
	return "Percona's ProxySQL"
}

// proxysqlAdminToolTag is the percona/proxysql-admin-tool release installed with upstream
// ProxySQL: the newest release tag of the major. Both read 8.0 and 8.4 clients; 9.x support is
// only on the unreleased v3.0.9-dev branch, so a PXC 9.x backend is refused for this source.
func proxysqlAdminToolTag(major string) string {
	if proxysqlMajorOf(major) == "3" {
		return "v3.0.6"
	}
	return "v2.7.3"
}

// loadUpstreamProxySQLCatalog parses the per-image upstream_proxysql section.
func loadUpstreamProxySQLCatalog() []PXCImage { return loadImageCatalog("upstream_proxysql") }

// proxysqlCatalogFor is the version catalogue a source installs from.
func proxysqlCatalogFor(src string) []PXCImage {
	if proxysqlIsUpstream(src) {
		return loadUpstreamProxySQLCatalog()
	}
	return loadProxySQLCatalog()
}

// proxysqlUpstreamVersions is what the upstream catalogue lists for one image and major,
// newest first, and whether it has the image at all.
func proxysqlUpstreamVersions(os, osVer, arch, major string) ([]string, bool) {
	for _, img := range loadUpstreamProxySQLCatalog() {
		if img.OS == os && img.OSVersion == osVer && (arch == "" || img.Arch == archOr(arch)) {
			return img.Versions[proxysqlMajorOf(major)], true
		}
	}
	return nil, false
}

// proxysqlUpstreamSeries is the repo.proxysql.com series directory ("3.0.x") a build comes
// from: the pinned version's, else the newest the catalogue lists for the image, else the
// newest series of the major.
func proxysqlUpstreamSeries(os, osVer, arch, major, version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		if vs, _ := proxysqlUpstreamVersions(os, osVer, arch, major); len(vs) > 0 {
			v = vs[0]
		}
	}
	if p := strings.SplitN(v, ".", 3); len(p) >= 2 && p[0] != "" && p[1] != "" {
		return p[0] + "." + p[1] + ".x"
	}
	if proxysqlMajorOf(major) == "3" {
		return "3.0.x"
	}
	return "2.7.x"
}

// proxysqlInstallStep is the install script, environment and a name for the log, for a
// ProxySQL instance's source.
func proxysqlInstallStep(src, os, osVer, arch, major, version string, useProxy bool, domain string) (string, []string, string) {
	debian := isDebianOS(os)
	if !proxysqlIsUpstream(src) {
		pkg := proxysqlPackage(proxysqlMajorOf(major))
		if debian {
			return proxysqlInstallDebian, []string{"PKG=" + pkg, "VER=" + version}, pkg
		}
		return proxysqlInstallRHEL, []string{"PKG=" + pkg, "VER=" + version}, pkg
	}
	series := proxysqlUpstreamSeries(os, osVer, arch, major, version)
	env := []string{"SERIES=" + series, "VER=" + strings.TrimSpace(version)}
	if useProxy {
		// dnf and apt are pointed at the proxy by pkgProxy*, but the signing key and the
		// admin tool are fetched with curl.
		env = append(env, "HTTPS_PROXY=http://intranet."+domain+":3128")
	}
	script := proxysqlInstallUpstreamRHEL
	if debian {
		script = proxysqlInstallUpstreamDebian
	}
	what := "proxysql (repo.proxysql.com, proxysql-" + series + ")"
	if strings.TrimSpace(src) == proxysqlSrcUpstreamAdmin {
		tag := proxysqlAdminToolTag(major)
		script += "\n" + proxysqlAdminToolScript
		env = append(env, "TAG="+tag)
		what += " and proxysql-admin " + tag
	}
	return script, env, what
}

// proxysqlRepoName is what the deployment records as the package's origin.
func proxysqlRepoName(src, os, osVer, arch, major, version string) string {
	if !proxysqlIsUpstream(src) {
		return "percona-release proxysql"
	}
	return "repo.proxysql.com proxysql-" + proxysqlUpstreamSeries(os, osVer, arch, major, version)
}

// proxysqlPXCNative reports whether a source configures a PXC backend with the native Galera
// script rather than proxysql-admin.
func proxysqlPXCNative(src string) bool {
	return strings.TrimSpace(src) == proxysqlSrcUpstreamNative
}

// proxysqlGaleraEnv is the environment for proxysqlGaleraConfigureScript.
func proxysqlGaleraEnv(sec proxysqlSecrets, members []string, mode string) []string {
	mon := sec.MonitorUser
	if mon == "" {
		mon = "monitor"
	}
	return []string{
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"MON_USER=" + mon, "MON_PW=" + sec.MonitorPassword,
		"SERVERS=" + strings.Join(members, ","),
		"MODE=" + proxysqlMode(mode),
	}
}

// pxcRegularFQDNs are the data members of a PXC frame (not arbitrators), by FQDN.
func pxcRegularFQDNs(doc designDoc, frame designFrame, domain string) []string {
	hosts := stackHostnames(doc)
	var out []string
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "pxc" && n.Role != "arbitrator" {
			out = append(out, fqdnOf(hosts[n.ID], domain))
		}
	}
	return out
}

// proxysqlSourceIssues checks a ProxySQL node's or cluster's source and version.
func proxysqlSourceIssues(what, src, os, osVer, arch, major, version string) []issue {
	var out []issue
	bad := func(format string, args ...any) {
		out = append(out, issue{Level: "error", Message: what + ": " + fmt.Sprintf(format, args...)})
	}
	src, version = strings.TrimSpace(src), strings.TrimSpace(version)
	switch src {
	case proxysqlSrcPercona, proxysqlSrcUpstreamAdmin, proxysqlSrcUpstreamNative:
	default:
		bad("unknown ProxySQL source %q — use \"\" (Percona), \"upstream-admin\" or \"upstream-native\"", src)
		return out
	}
	if !proxysqlIsUpstream(src) || version == "" {
		return out
	}
	if !strings.HasPrefix(version, proxysqlMajorOf(major)+".") {
		bad("ProxySQL %s is not in the %s series", version, proxysqlMajorOf(major))
		return out
	}
	vs, ok := proxysqlUpstreamVersions(os, osVer, arch, major)
	if !ok || len(vs) == 0 {
		return out // nothing recorded for the image: the install is the check
	}
	for _, v := range vs {
		if v == version {
			return out
		}
	}
	bad("repo.proxysql.com has no ProxySQL %s for %s %s — the %s series there runs %s to %s", version, os, osVer, proxysqlMajorOf(major), vs[len(vs)-1], vs[0])
	return out
}

// proxysqlInstallUpstreamRHEL/Debian add repo.proxysql.com's repository for SERIES and install
// proxysql (VER pinned, or the newest of the series), then check the package really came from
// there: Percona's proxysql2/proxysql3 also provide "proxysql".
const proxysqlInstallUpstreamRHEL = `set -e
export https_proxy="${HTTPS_PROXY:-}"
elver=$(rpm -E %rhel)
cat > /etc/yum.repos.d/proxysql-upstream.repo <<REPO
[proxysql-upstream]
name=ProxySQL $SERIES (repo.proxysql.com)
baseurl=https://repo.proxysql.com/ProxySQL/proxysql-$SERIES/almalinux/$elver
gpgcheck=1
gpgkey=https://repo.proxysql.com/ProxySQL/proxysql-$SERIES/repo_pub_key
REPO
spec=proxysql
[ -n "$VER" ] && spec="proxysql-$VER"
# which: proxysql-admin shells out to it, and a minimal EL image has none.
dnf -y -q install "$spec" which >/dev/null
from=$(dnf -q info --installed proxysql 2>/dev/null | awk -F': ' '/^From repo/{print $2; exit}')
[ "$from" = proxysql-upstream ] || { echo "proxysql was installed from ${from:-an unknown repository}, not repo.proxysql.com" >&2; exit 1; }`

const proxysqlInstallUpstreamDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
export https_proxy="${HTTPS_PROXY:-}"
command -v gpg >/dev/null 2>&1 && command -v curl >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq gnupg curl ca-certificates >/dev/null; }
install -d /etc/apt/keyrings
curl -fsSL --retry 3 "https://repo.proxysql.com/ProxySQL/proxysql-$SERIES/repo_pub_key" | gpg --batch --yes --dearmor -o /etc/apt/keyrings/proxysql-upstream.gpg
CODE="$(. /etc/os-release; echo "$VERSION_CODENAME")"
echo "deb [signed-by=/etc/apt/keyrings/proxysql-upstream.gpg] https://repo.proxysql.com/ProxySQL/proxysql-$SERIES/$CODE/ ./" > /etc/apt/sources.list.d/proxysql-upstream.list
apt-get update -qq >/dev/null
exact=$(apt-cache madison proxysql 2>/dev/null | awk -F'|' -v v="$VER" '$3 ~ /repo\.proxysql\.com/ {gsub(/ /,"",$2); if (v == "" || $2 == v) {print $2; exit}}')
[ -n "$exact" ] || { echo "repo.proxysql.com proxysql-$SERIES has no proxysql ${VER:-build} for $CODE" >&2; exit 1; }
apt-get install -y -qq "proxysql=$exact" debianutils >/dev/null`

// proxysqlAdminToolScript installs proxysql-admin and its helpers from a release tag of
// percona/proxysql-admin-tool, where an upstream ProxySQL package has none. The .cnf is the
// template proxysqlConfigureScript then fills in, as it does Percona's.
const proxysqlAdminToolScript = `D=$(mktemp -d)
curl -fsSL --retry 3 "https://github.com/percona/proxysql-admin-tool/archive/refs/tags/$TAG.tar.gz" | tar xz -C "$D"
T=$(echo "$D"/proxysql-admin-tool-*)
for f in proxysql-admin proxysql-admin-common proxysql-common proxysql-login-file proxysql-status; do
  install -m 0755 "$T/$f" "/usr/bin/$f"
done
[ -f /etc/proxysql-admin.cnf ] || install -m 0640 "$T/proxysql-admin.cnf" /etc/proxysql-admin.cnf
rm -rf "$D"`

// proxysqlGaleraConfigureScript configures a PXC backend natively over the admin interface,
// with the host groups and rules proxysql-admin sets up, so the two upstream options route
// alike: writer 10, reader 11, backup writer 12, offline 13, at most 100 transactions behind.
// singlewrite is one writer whose backups also take reads (writer_is_also_reader=2) and the
// app user's SELECTs go to the readers, SELECT … FOR UPDATE to the writer; loadbal makes every
// member a writer with no rules. SERVERS is the members' FQDNs.
const proxysqlGaleraConfigureScript = `set -e
A="mysql -u$ADMIN_USER -p$ADMIN_PW -h127.0.0.1 -P6032 --protocol=tcp"
$A -e "UPDATE global_variables SET variable_value='$MON_USER' WHERE variable_name='mysql-monitor_username';
UPDATE global_variables SET variable_value='$MON_PW' WHERE variable_name='mysql-monitor_password';
LOAD MYSQL VARIABLES TO RUNTIME; SAVE MYSQL VARIABLES TO DISK;"
if [ "$MODE" = loadbal ]; then MAXW=1000000; WAR=0; else MAXW=1; WAR=2; fi
$A -e "DELETE FROM mysql_galera_hostgroups;
INSERT INTO mysql_galera_hostgroups (writer_hostgroup,backup_writer_hostgroup,reader_hostgroup,offline_hostgroup,active,max_writers,writer_is_also_reader,max_transactions_behind,comment)
VALUES (10,12,11,13,1,$MAXW,$WAR,100,'pxc');
LOAD MYSQL SERVERS TO RUNTIME; SAVE MYSQL SERVERS TO DISK;"
SQL="DELETE FROM mysql_servers;"
for h in $(echo "$SERVERS" | tr ',' ' '); do SQL="$SQL INSERT INTO mysql_servers (hostgroup_id,hostname,port,weight,max_connections) VALUES (10,'$h',3306,1000,1000);"; done
$A -e "$SQL LOAD MYSQL SERVERS TO RUNTIME; SAVE MYSQL SERVERS TO DISK;"
$A -e "DELETE FROM mysql_users WHERE username='$APP_USER';
INSERT INTO mysql_users (username,password,default_hostgroup,active) VALUES ('$APP_USER','$APP_PW',10,1);
LOAD MYSQL USERS TO RUNTIME; SAVE MYSQL USERS TO DISK;"
RULES="DELETE FROM mysql_query_rules WHERE username='$APP_USER';"
if [ "$MODE" != loadbal ]; then
  RULES="$RULES INSERT INTO mysql_query_rules (username,destination_hostgroup,active,match_digest,apply) VALUES ('$APP_USER',10,1,'^SELECT.*FOR UPDATE',1),('$APP_USER',11,1,'^SELECT ',1);"
fi
$A -e "$RULES LOAD MYSQL QUERY RULES TO RUNTIME; SAVE MYSQL QUERY RULES TO DISK;"`

// composeProxySQLSource reads compose's source= on a proxysql entry ("percona" is "").
func composeProxySQLSource(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "percona":
		return proxysqlSrcPercona, nil
	case proxysqlSrcUpstreamAdmin, proxysqlSrcUpstreamNative:
		return v, nil
	}
	return "", fmt.Errorf("source=%s: want percona, upstream-admin or upstream-native", s)
}
