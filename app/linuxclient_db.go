package main

import (
	"context"
	"slices"
	"strings"
)

// linuxclient_db.go — Percona's database client tools on a Linux Client node.
//
// The Linux Client is the jump box, and the first thing anyone does on one is install a client to
// talk to the stack's databases. Five of them are a design-time choice, all from Percona's own
// repositories through percona-release (already on every base image):
//
//	mysql           percona-server-client        ps-80 / ps-84-lts / ps-97-lts
//	mysqlsh         percona-mysql-shell          the same series repository as the client
//	mongosh         percona-mongodb-mongosh      psmdb-80 (psmdb-70 on CentOS 7)
//	psql            percona-postgresql-client-NN / percona-postgresqlNN   ppg-NN
//	pcsm            percona-clustersync-mongodb  pcsm
//
// ------------------------------------------------------------------- what each release carries
//
// Probed against every Linux Client release before this was written, not assumed:
//
//   - Debian 13 (trixie) has no 8.0 percona-server-client — ps-80 publishes only the shell there —
//     so its MySQL series are 8.4 and 9.7.
//   - CentOS 7's last Percona builds are 8.0.37 (client and shell), mongosh 2.1.5 from psmdb-70
//     (psmdb-80 has no el7 build), and psql 13 — 14 to 16 exist for el7 but need libzstd, which
//     CentOS 7 never shipped (see scPgClientMajor). ClusterSync was never built for el7.
//   - Oracle Linux 8 hides percona-server-client and percona-postgresqlNN behind the AppStream
//     mysql and postgresql modules, so both are disabled first (as the Percona Server and
//     PostgreSQL nodes do).
//
// lcMySQLSeries / lcPsqlSeries / lcClusterSyncOn say so in one place, the designer offers only
// what they allow, and validateStack refuses a design that asks for anything else — a package step
// that fails with "Unable to locate package" ten times over is the worst way to find out.
//
// Like the Kubernetes tools, a failed install never fails the deploy: the node is still a jump box,
// and its terminal is right there. What it must not do is *report* a tool it does not have, so what
// is recorded is read back off the binaries afterwards.

// lcDBTool is one installable client, as the designer and the deployed panel name it.
type lcDBTool struct {
	ID    string // the binary, and the id the panel keys on
	Label string
}

var (
	lcToolMySQL       = lcDBTool{"mysql", "Percona Server MySQL client"}
	lcToolMySQLShell  = lcDBTool{"mysqlsh", "MySQL Shell"}
	lcToolMongosh     = lcDBTool{"mongosh", "mongosh"}
	lcToolPsql        = lcDBTool{"psql", "psql"}
	lcToolClusterSync = lcDBTool{"pcsm", "Percona ClusterSync for MongoDB"}
)

// lcDBClient is a client tool as it is ON THE NODE, read back after the install.
type lcDBClient struct {
	Tool    string `json:"tool"`
	Label   string `json:"label"`
	Version string `json:"version"`
}

// lcMySQLSeries is the Percona Server client series a release can install, oldest first.
func lcMySQLSeries(os, osVersion string) []string {
	switch {
	case isEL7OS(os):
		return []string{"8.0"}
	case os == "debian" && osVersion == "13":
		return []string{"8.4", "9.7"}
	}
	return []string{"8.0", "8.4", "9.7"}
}

// lcPsqlSeries is the Percona Distribution for PostgreSQL client series a release can install.
func lcPsqlSeries(os string) []string {
	if isEL7OS(os) {
		return []string{"13"}
	}
	return []string{"13", "14", "15", "16", "17", "18"}
}

// lcClusterSyncOn reports whether Percona publishes ClusterSync for a release.
func lcClusterSyncOn(os string) bool { return !isEL7OS(os) }

// lcMySQLMajor is the series a node installs: what it picked, or 8.4 (the LTS) when it picked
// nothing — or the release's only one, on CentOS 7.
func lcMySQLMajor(n designNode) string {
	if m := strings.TrimSpace(n.LCMySQLMajor); m != "" {
		return m
	}
	if s := lcMySQLSeries(n.OS, n.OSVersion); !slices.Contains(s, "8.4") {
		return s[len(s)-1]
	}
	return "8.4"
}

// lcPsqlMajor is the PostgreSQL series a node installs: what it picked, or 17 by default.
func lcPsqlMajor(n designNode) string {
	if m := strings.TrimSpace(n.LCPsqlMajor); m != "" {
		return m
	}
	if s := lcPsqlSeries(n.OS); !slices.Contains(s, "17") {
		return s[len(s)-1]
	}
	return "17"
}

// lcWantsDBClients reports whether a node asked for any of the database clients.
func lcWantsDBClients(n designNode) bool {
	return n.LCMySQLClient || n.LCMySQLShell || n.LCMongosh || n.LCPsql || n.LCClusterSync
}

// lcDBClientIssues is what validateStack refuses about a node's database clients: a series its
// release does not carry, or ClusterSync where there is none.
func lcDBClientIssues(n designNode) []issue {
	var out []issue
	osName := n.OS + " " + n.OSVersion
	if isEL7OS(n.OS) {
		osName = "CentOS 7"
	}
	if n.LCMySQLClient || n.LCMySQLShell {
		if m, s := lcMySQLMajor(n), lcMySQLSeries(n.OS, n.OSVersion); !slices.Contains(s, m) {
			out = append(out, issue{Level: "error", Message: "Linux Client " + n.Label + ": Percona publishes no MySQL " + m +
				" client for " + osName + " — pick " + strings.Join(s, " or ")})
		}
	}
	if n.LCPsql {
		if m, s := lcPsqlMajor(n), lcPsqlSeries(n.OS); !slices.Contains(s, m) {
			out = append(out, issue{Level: "error", Message: "Linux Client " + n.Label + ": psql " + m +
				" does not install on " + osName + " — pick " + strings.Join(s, ", ")})
		}
	}
	if n.LCClusterSync && !lcClusterSyncOn(n.OS) {
		out = append(out, issue{Level: "error", Message: "Linux Client " + n.Label +
			": Percona publishes no ClusterSync for MongoDB build for " + osName + " — pick another release"})
	}
	return out
}

// lcDBStep is one package-manager run: the repositories to enable, then the packages. Each client
// is its own step so one that fails does not take the others with it.
type lcDBStep struct {
	Tools    []lcDBTool
	Repos    []string // percona-release repositories, enabled additively
	HandRepo string   // a series percona-release cannot enable; written by hand (see psRepoRHEL)
	Packages []string
}

// lcDBSteps is what a node's choices install, in the order they run.
func lcDBSteps(n designNode) []lcDBStep {
	deb, el7 := isDebianOS(n.OS), isEL7OS(n.OS)
	var out []lcDBStep
	if n.LCMySQLClient || n.LCMySQLShell {
		m := lcMySQLMajor(n)
		st := lcDBStep{}
		if p := psClientProduct(m); p == "" {
			st.HandRepo = psRepoName(m)
		} else {
			st.Repos = []string{psRepoName(m)}
		}
		if n.LCMySQLClient {
			st.Tools = append(st.Tools, lcToolMySQL)
			st.Packages = append(st.Packages, "percona-server-client")
		}
		if n.LCMySQLShell {
			st.Tools = append(st.Tools, lcToolMySQLShell)
			st.Packages = append(st.Packages, "percona-mysql-shell")
		}
		out = append(out, st)
	}
	if n.LCMongosh {
		repo := "psmdb-80"
		if el7 {
			repo = "psmdb-70"
		}
		out = append(out, lcDBStep{Tools: []lcDBTool{lcToolMongosh}, Repos: []string{repo}, Packages: []string{"percona-mongodb-mongosh"}})
	}
	if n.LCPsql {
		m := lcPsqlMajor(n)
		pkg := "percona-postgresql" + m
		if deb {
			pkg = "percona-postgresql-client-" + m
		}
		out = append(out, lcDBStep{Tools: []lcDBTool{lcToolPsql}, Repos: []string{ppgProduct(m)}, Packages: []string{pkg}})
	}
	if n.LCClusterSync {
		out = append(out, lcDBStep{Tools: []lcDBTool{lcToolClusterSync}, Repos: []string{"pcsm"}, Packages: []string{"percona-clustersync-mongodb"}})
	}
	return out
}

// lcDBRepos enables the repositories a step needs. `enable`, not `setup`: setup disables every
// other Percona repository first, which would undo the step before it.
const lcDBRepos = `if [ -n "$PROXY" ]; then
  export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"
  export no_proxy="localhost,127.0.0.1,.$DOMAIN" NO_PROXY="$no_proxy"
fi
for r in $REPOS; do
  percona-release enable "$r" release >/dev/null 2>&1 || { echo "percona-release could not enable $r"; exit 1; }
  echo "enabled $r"
done
`

// lcDBHandRepo{RHEL,Debian} wrap the hand-written repository the Percona Server nodes use for a
// series percona-release cannot enable (psRepoRHEL), keyed on $HAND_REPO.
const lcDBHandRepoRHEL = `if [ -n "$HAND_REPO" ]; then
PRODUCT=""; REPO="$HAND_REPO"
` + psRepoRHEL + `echo "wrote the $HAND_REPO repository"
fi
`

const lcDBHandRepoDebian = `if [ -n "$HAND_REPO" ]; then
PRODUCT=""; REPO="$HAND_REPO"
` + psRepoDebian + `echo "wrote the $HAND_REPO repository"
fi
`

// Oracle Linux 8's AppStream modules hide percona-server-client and percona-postgresqlNN; the
// disable is a no-op on 9 and 10.
const lcDBInstallRHEL = `set -e
dnf -y -q module disable mysql postgresql >/dev/null 2>&1 || true
` + lcDBHandRepoRHEL + lcDBRepos + `dnf -y install $PKGS
`

const lcDBInstallEL7 = `set -e
` + lcDBRepos + `yum -y install $PKGS
`

const lcDBInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
` + lcDBHandRepoDebian + lcDBRepos + `apt-get update -qq
apt-get install -y --no-install-recommends $PKGS
`

// lcPsqlPath puts an EL psql on PATH. The RPMs install under /usr/pgsql-NN/bin and link nothing
// into /usr/bin; Debian's postgresql-common wrapper already does.
const lcPsqlPath = `printf 'export PATH=%s:$PATH\n' "$BINDIR" > /etc/profile.d/percona-psql.sh`

// lcDBVersionCmd is how each tool states its own version, as one line.
var lcDBVersionCmd = map[string]string{
	"mysql":   `mysql --version | grep -oE 'Ver [^ ]+' | head -1 | cut -d' ' -f2`,
	"mysqlsh": `mysqlsh --version 2>/dev/null | grep -oE 'Ver [^ ]+' | head -1 | cut -d' ' -f2`,
	"mongosh": `mongosh --version 2>/dev/null | tail -1`,
	"psql":    `psql --version | awk '{print $3}'`,
	// pcsm prints its version on stderr.
	"pcsm": `pcsm version 2>&1 | awk '/^Version:/{print $2; exit}'`,
}

// linuxClientInstallDBClients installs what the node asked for and records what landed. It never
// fails the deploy — see the file comment.
func (a *App) linuxClientInstallDBClients(ctx context.Context, id string, n designNode, cfg *linuxClientConfig, pr *pxcProg) {
	steps := lcDBSteps(n)
	if len(steps) == 0 {
		return
	}
	var names []string
	for _, st := range steps {
		for _, t := range st.Tools {
			names = append(names, t.ID)
		}
	}
	pr.phase("Installing "+strings.Join(names, ", "), 88)

	script := lcDBInstallRHEL
	switch {
	case isDebianOS(n.OS):
		script = lcDBInstallDebian
	case isEL7OS(n.OS):
		script = lcDBInstallEL7
	}
	domain := envOr("DOMAIN", "example.net")
	proxy := ""
	if n.UseProxy {
		proxy = "http://intranet." + domain + ":3128"
	}
	for _, st := range steps {
		repos := slices.Clone(st.Repos)
		if st.HandRepo != "" {
			repos = append(repos, st.HandRepo)
		}
		pr.logln("installing " + strings.Join(st.Packages, " ") + " from " + strings.Join(repos, ", "))
		env := []string{
			"PKGS=" + strings.Join(st.Packages, " "),
			"REPOS=" + strings.Join(st.Repos, " "),
			"HAND_REPO=" + st.HandRepo,
			"PROXY=" + proxy, "DOMAIN=" + domain,
		}
		if err := a.runStep(ctx, id, script, env, pr.logln); err != nil {
			pr.logln(strings.Join(st.Packages, " ") + " did not install: " + lastLines(err.Error(), 200) +
				" — the node is up regardless; install it from its terminal")
		}
	}
	if n.LCPsql && !isDebianOS(n.OS) {
		bin := pgBinDir(n.OS, lcPsqlMajor(n))
		if err := a.runStep(ctx, id, lcPsqlPath, []string{"BINDIR=" + bin}, pr.logln); err != nil {
			pr.logln("could not put " + bin + " on PATH: " + err.Error())
		}
	}

	// Read back what is on the node. A login shell, so the psql PATH entry above applies.
	cfg.DBClients = nil
	for _, st := range steps {
		for _, t := range st.Tools {
			v := a.lcToolVersion(ctx, id, lcDBVersionCmd[t.ID])
			if v == "" {
				pr.logln(t.ID + " did not install")
				continue
			}
			cfg.DBClients = append(cfg.DBClients, lcDBClient{Tool: t.ID, Label: t.Label, Version: v})
			pr.logln(t.ID + " " + v + " on PATH")
		}
	}
}
