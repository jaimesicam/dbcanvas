package main

import (
	"fmt"
	"strings"
)

// samplecode_env.go — from "this sample needs Python and a MySQL driver" to "run these commands on
// this node".
//
// The indirection here is the point, and it is the one the feature would be worst without:
//
//	sample requirement  →  dependency resolver  →  Linux distribution package installer
//
// A sample declares that it needs `python3` and `mysql-connector-python`. It never says `dnf` and
// it never says `apt-get`, because a Linux Client is any OS image DBCanvas builds — Oracle Linux
// 8/9/10, Ubuntu 22.04/24.04, Debian 12/13 — and a sample that knew which one it was running on
// would have to be rewritten the first time a new base image is added. Only scSysPackages below
// knows about package managers, and it is the only thing that has to change.
//
// The second rule is the one that makes the feature usable more than once: **check before you
// install**. Every step carries a check that is cheap and true — `command -v node`, an import in
// the virtualenv, a directory in node_modules — and a step whose check passes is reported as
// already installed and skipped. Running the same sample twice installs nothing the second time,
// and the ecosystems' own caches (pip's virtualenv, node_modules, the Go module cache, ~/.m2) sit
// under one directory that outlives any single run.
//
// The third rule is that none of it is hidden. Every command that runs is echoed into the job log
// before it runs, and its output follows. DBCanvas is a lab: "what did it actually do" is not a
// debugging question here, it is the product.

// Runtimes. A client declares one; it decides which interpreter/compiler is installed and how the
// project's own dependencies are resolved.
const (
	scRuntimePython = "python"
	scRuntimeNode   = "node"
	scRuntimeGo     = "go"
	scRuntimeJava   = "java"
	scRuntimeShell  = "shell"
)

// scRoot is where every generated project lives on the Linux Client, and why it is one directory:
// the caches under it (.venv, the Go module cache, ~/.m2 by way of $HOME) are what make the second
// run of a sample instant. Reset empties one project, never the caches.
const scRoot = "/root/dbcanvas-samples"

// scVenv is the Python virtualenv shared by every Python sample on a node.
//
// A virtualenv rather than `pip install` into the system Python, for a reason that is not
// stylistic: Debian and Ubuntu mark their system Python as externally managed (PEP 668) and pip
// refuses to write to it, so the obvious command fails on three of the six base images. It is also
// the only arrangement in which "is mysql-connector-python installed" has one answer rather than
// one per user.
const scVenv = scRoot + "/.venv"

// scStorePass is the password on the PKCS#12 stores generated for the Java samples. It protects
// nothing — the store holds a public CA certificate, and the client key beside it is already
// readable by root on this node — and a JKS/PKCS12 store cannot be created without one. Spelled
// out here rather than invented per run so the generated code, the log and the file agree.
const scStorePass = "changeit"

// scSysPkg is one logical system package: a runtime, a native client, or a tool a sample needs.
//
// Packages and Repo are functions because two of these are not constants. The native database
// clients come from Percona's repositories, and *which* repository depends on the series the
// target runs — a psql from the PostgreSQL 17 distribution and one from 13 are different packages
// in different repos, and on EL they are not even on PATH in the same place.
type scSysPkg struct {
	ID    string
	Label string
	// Check is a shell test: exit 0 means the package is already usable. It tests the *tool*,
	// not the package database, because "the rpm is installed" and "the command runs" are
	// different statements and only the second one matters here.
	Check func(nodeOS string, t scTarget) string
	// Packages is what to install, per OS family.
	Packages func(nodeOS string, t scTarget) []string
	// Alt is a second list to try when Packages fails, for the one case where a package name
	// genuinely differs between releases of the same family (the JDK on EL8 vs EL9+).
	Alt func(nodeOS string, t scTarget) []string
	// Repo is the percona-release product to enable before installing, or "" for the distro's
	// own repositories.
	Repo func(t scTarget) string
	// License and URL describe what is being installed, for the same reason scDep carries them.
	License string
	URL     string
}

// scSysPackages is every system package this feature can install, and the only place in it that
// knows a package manager exists.
var scSysPackages = map[string]scSysPkg{
	"python3": {
		ID: "python3", Label: "Python 3 (with pip and venv)",
		// ensurepip is what `python3 -m venv` needs and what Debian splits into python3-venv.
		// Testing for the interpreter alone passes on a node where the virtualenv cannot be made.
		Check: func(string, scTarget) string { return `python3 -c "import venv, ensurepip"` },
		Packages: func(nodeOS string, _ scTarget) []string {
			if isDebianOS(nodeOS) {
				return []string{"python3", "python3-pip", "python3-venv"}
			}
			return []string{"python3", "python3-pip"}
		},
		License: "PSF-2.0", URL: "https://www.python.org/",
	},
	"nodejs": {
		ID: "nodejs", Label: "Node.js (with npm)",
		Check:    func(string, scTarget) string { return `command -v node >/dev/null && command -v npm >/dev/null` },
		Packages: func(string, scTarget) []string { return []string{"nodejs", "npm"} },
		License:  "MIT", URL: "https://nodejs.org/",
	},
	"golang": {
		ID: "golang", Label: "Go toolchain",
		Check: func(string, scTarget) string { return `command -v go >/dev/null` },
		Packages: func(nodeOS string, _ scTarget) []string {
			if isDebianOS(nodeOS) {
				return []string{"golang-go"}
			}
			return []string{"golang"}
		},
		License: "BSD-3-Clause", URL: "https://go.dev/",
	},
	"jdk": {
		ID: "jdk", Label: "JDK (OpenJDK)",
		Check: func(string, scTarget) string { return `command -v javac >/dev/null` },
		Packages: func(nodeOS string, _ scTarget) []string {
			if isDebianOS(nodeOS) {
				return []string{"default-jdk"}
			}
			return []string{"java-21-openjdk-devel"}
		},
		// EL8 has no java-21 package; 17 is the LTS it ships. Tried only if the first list
		// fails, so a node that has 21 gets 21.
		Alt: func(nodeOS string, _ scTarget) []string {
			if isDebianOS(nodeOS) {
				return nil
			}
			return []string{"java-17-openjdk-devel"}
		},
		License: "GPL-2.0-only WITH Classpath-exception-2.0", URL: "https://openjdk.org/",
	},
	"maven": {
		ID: "maven", Label: "Apache Maven",
		Check:    func(string, scTarget) string { return `command -v mvn >/dev/null` },
		Packages: func(string, scTarget) []string { return []string{"maven"} },
		License:  "Apache-2.0", URL: "https://maven.apache.org/",
	},
	"openssl": {
		ID: "openssl", Label: "OpenSSL command line",
		Check:    func(string, scTarget) string { return `command -v openssl >/dev/null` },
		Packages: func(string, scTarget) []string { return []string{"openssl"} },
		License:  "Apache-2.0", URL: "https://www.openssl.org/",
	},
	"mysql-client": {
		ID: "mysql-client", Label: "mysql (Percona Server client)",
		Check:    func(string, scTarget) string { return `command -v mysql >/dev/null` },
		Packages: func(string, scTarget) []string { return []string{"percona-server-client"} },
		// Same repository the ProxySQL node uses for the same binary — the series the target
		// runs, so the client is never older than the server it is pointed at.
		Repo:    func(t scTarget) string { return psClientProduct(psMajorOf(scMajorOr(t.Major, "8.0"))) },
		License: "GPL-2.0-only", URL: "https://www.percona.com/mysql",
	},
	"psql-client": {
		ID: "psql-client", Label: "psql (Percona Distribution for PostgreSQL client)",
		// On EL the client lands under /usr/pgsql-NN/bin and is not on PATH; on Debian it is.
		// Both are accepted, and the generated script resolves it the same way.
		Check: func(nodeOS string, t scTarget) string {
			return `command -v psql >/dev/null || [ -x ` + scPgBinDir(nodeOS, t) + `/psql ]`
		},
		Packages: func(nodeOS string, t scTarget) []string {
			m := ppgMajorOf(scMajorOr(t.Major, "17"))
			if isDebianOS(nodeOS) {
				return []string{"percona-postgresql-client-" + m}
			}
			return []string{"percona-postgresql" + m}
		},
		Repo:    func(t scTarget) string { return ppgProduct(scMajorOr(t.Major, "17")) },
		License: "PostgreSQL", URL: "https://www.percona.com/postgresql",
	},
	"mongosh": {
		ID: "mongosh", Label: "mongosh (MongoDB Shell)",
		Check:    func(string, scTarget) string { return `command -v mongosh >/dev/null` },
		Packages: func(string, scTarget) []string { return []string{"percona-mongodb-mongosh"} },
		Repo:     func(t scTarget) string { return psmdbRepo(scMajorOr(t.Major, "8.0")) },
		License:  "Apache-2.0", URL: "https://github.com/mongodb-js/mongosh",
	},
	"valkey-cli": {
		ID: "valkey-cli", Label: "valkey-cli",
		Check: func(string, scTarget) string { return `command -v valkey-cli >/dev/null` },
		Packages: func(nodeOS string, _ scTarget) []string {
			// Debian splits the CLI tools out of the server package; EL bundles them.
			// Same split valkeyPackages documents for the Valkey node itself.
			if isDebianOS(nodeOS) {
				return []string{"percona-valkey-tools"}
			}
			return []string{"percona-valkey"}
		},
		Repo:    func(scTarget) string { return "valkey-91" },
		License: "BSD-3-Clause", URL: "https://valkey.io/",
	},
}

// scRuntimePackages is the system packages a runtime needs, before anything the sample adds.
func scRuntimePackages(runtime string) []string {
	switch runtime {
	case scRuntimePython:
		return []string{"python3"}
	case scRuntimeNode:
		return []string{"nodejs"}
	case scRuntimeGo:
		return []string{"golang"}
	case scRuntimeJava:
		return []string{"jdk", "maven"}
	}
	return nil // shell: the native client is declared by the sample itself
}

// scMajorOr returns the target's product series, or a default when the deployment did not record
// one. The default is stated rather than guessed at the call site so a node whose config predates
// the field still installs something that works.
func scMajorOr(major, def string) string {
	if m := strings.TrimSpace(major); m != "" {
		return m
	}
	return def
}

// scPgBinDir is where the Percona PostgreSQL client binaries land on this node's OS.
func scPgBinDir(nodeOS string, t scTarget) string {
	return pgBinDir(nodeOS, ppgMajorOf(scMajorOr(t.Major, "17")))
}

// scRequirementLabels is what a client needs, in the order the environment log will report it.
// Shown in the picker before anything is installed.
func scRequirementLabels(c scClient) []string {
	var out []string
	for _, id := range append(scRuntimePackages(c.Runtime), c.SysPackages...) {
		if p, ok := scSysPackages[id]; ok {
			out = append(out, p.Label)
		}
	}
	for _, d := range c.Deps {
		out = append(out, d.Name+" ("+scManagerLabel(d.Manager)+")")
	}
	return out
}

func scManagerLabel(m string) string {
	switch m {
	case "pip":
		return "pip"
	case "npm":
		return "npm"
	case "gomod":
		return "Go module"
	case "maven":
		return "Maven"
	}
	return m
}

// ---------------------------------------------------------------------------------- the plan

// scStep is one thing the runner does, and one line (at least) in the log.
//
// Check is what makes a step idempotent: when it exits 0 the step is reported as already
// satisfied and Cmd never runs. Show is the command line the user is told about — it is separate
// from Cmd because Cmd carries the shell scaffolding (set -e, the proxy exports, the fallback
// package list) that would only be noise, and because a step must never echo a secret.
type scStep struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Kind  string   `json:"kind"` // system | dep | prepare | run
	Check string   `json:"-"`
	Cmd   string   `json:"-"`
	Show  string   `json:"show"`
	Dir   string   `json:"-"`
	Env   []string `json:"-"`
}

// scPlan is everything that has to happen for a sample to run, in order.
type scPlan struct {
	Dir     string   `json:"dir"`
	System  []scStep `json:"system"`  // runtimes and native clients, via the package manager
	Files   []scFile `json:"-"`       // written by DBCanvas, not by a shell command
	Deps    []scStep `json:"deps"`    // the ecosystem's own resolver, inside the project
	Prepare []scStep `json:"prepare"` // key/trust stores and the like, after the files exist
	Run     scStep   `json:"run"`
}

// scProjectDir is where one sample's project lives: one directory per sample address, so two
// samples never fight over a package.json and a Reset can empty one without touching the other.
func scProjectDir(sampleID string) string {
	return scRoot + "/" + strings.ReplaceAll(sampleID, "/", "-")
}

// scInstallScript is the whole of this feature's knowledge of package managers.
//
// $REPO, when set, is a percona-release product: the same enable-then-setup pair every product's
// install script in this codebase uses, because `percona-release enable` is the cheap path and
// `setup` is the one that works on a node where the repo was never configured.
//
// $ALT is the fallback package list. It exists for exactly one case (the JDK, which is java-21 on
// EL9+ and java-17 on EL8) and is empty everywhere else; a failure with no $ALT is a failure.
const scInstallRHEL = `set -e
if [ -n "$PROXY" ]; then export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"; fi
if [ -n "$REPO" ]; then
  percona-release enable "$REPO" >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1 || true
fi
if ! dnf -y install $PKGS; then
  [ -n "$ALT" ] || exit 1
  echo "falling back to: $ALT"
  dnf -y install $ALT
fi`

const scInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
if [ -n "$PROXY" ]; then export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"; fi
if [ -n "$REPO" ]; then
  percona-release enable "$REPO" >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1 || true
fi
apt-get update -qq
if ! apt-get install -y --no-install-recommends $PKGS; then
  [ -n "$ALT" ] || exit 1
  echo "falling back to: $ALT"
  apt-get install -y --no-install-recommends $ALT
fi`

// scBuildPlan turns a resolved sample into the ordered work the node has to do.
func scBuildPlan(c scClient, g scGen, nodeOS string, useProxy bool) scPlan {
	plan := scPlan{Dir: g.Dir, Files: c.Files(g)}
	proxy := ""
	if useProxy {
		proxy = "http://intranet." + envOr("DOMAIN", "example.net") + ":3128"
	}

	// 1. System packages: the runtime, then whatever native client the sample named — plus
	//    openssl where a prepare step is about to need it, which is only the JVM's client
	//    certificate conversions. It is on every base image in practice; asking for it is a
	//    line, and a prepare step failing on a missing binary is a support question.
	need := append(scRuntimePackages(c.Runtime), c.SysPackages...)
	if c.Runtime == scRuntimeJava && g.MTLS() {
		need = append(need, "openssl")
	}
	for _, id := range need {
		p, ok := scSysPackages[id]
		if !ok {
			continue
		}
		pkgs := strings.Join(p.Packages(nodeOS, g.Target), " ")
		alt := ""
		if p.Alt != nil {
			alt = strings.Join(p.Alt(nodeOS, g.Target), " ")
		}
		repo := ""
		if p.Repo != nil {
			repo = p.Repo(g.Target)
		}
		script := scInstallRHEL
		show := "dnf -y install " + pkgs
		if isDebianOS(nodeOS) {
			script, show = scInstallDebian, "apt-get install -y "+pkgs
		}
		if repo != "" {
			show = "percona-release enable " + repo + " && " + show
		}
		plan.System = append(plan.System, scStep{
			ID: "sys:" + id, Label: p.Label, Kind: "system",
			Check: p.Check(nodeOS, g.Target), Cmd: script, Show: show,
			Env: []string{"PKGS=" + pkgs, "ALT=" + alt, "REPO=" + repo, "PROXY=" + proxy},
		})
	}

	// 2. The ecosystem's own dependency resolver, run inside the project.
	env := []string{}
	if proxy != "" {
		env = append(env,
			"http_proxy="+proxy, "https_proxy="+proxy,
			"HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy,
			"no_proxy=localhost,127.0.0.1,."+envOr("DOMAIN", "example.net"))
	}
	plan.Deps = append(plan.Deps, scDepSteps(c, g, env)...)

	// 3. Anything that has to exist beside the source before it will run. Only Java needs this,
	//    and only for TLS: the JVM reads trust material out of a keystore, never a PEM.
	plan.Prepare = scPrepareSteps(c, g)

	// 4. The program itself.
	plan.Run = scStep{
		ID: "run", Label: "Run " + c.Label, Kind: "run",
		Cmd: c.Run(g), Show: c.Run(g), Dir: g.Dir, Env: env,
	}
	return plan
}

// scDepSteps is the per-ecosystem half of the resolver: pip into the shared virtualenv, npm into
// the project's node_modules, `go mod tidy` against the module cache, Maven into ~/.m2. Each one
// is the tool the ecosystem expects a developer to use, and each one is skipped when its own check
// says the work is already done.
func scDepSteps(c scClient, g scGen, env []string) []scStep {
	var out []scStep
	switch c.Runtime {
	case scRuntimePython:
		out = append(out, scStep{
			ID: "venv", Label: "Python virtualenv", Kind: "dep",
			Check: "[ -x " + scVenv + "/bin/python ]",
			Cmd:   "set -e\npython3 -m venv " + scVenv,
			Show:  "python3 -m venv " + scVenv,
			Env:   env,
		})
		for _, d := range c.Deps {
			if d.Manager != "pip" {
				continue
			}
			imp := d.Import
			if imp == "" {
				imp = d.Name
			}
			out = append(out, scStep{
				ID: "pip:" + d.Name, Label: d.Name, Kind: "dep",
				Check: scVenv + `/bin/python -c "import ` + imp + `"`,
				Cmd:   "set -e\n" + scVenv + "/bin/pip install --disable-pip-version-check --no-input " + d.Name,
				Show:  scVenv + "/bin/pip install " + d.Name,
				Env:   env,
			})
		}
	case scRuntimeNode:
		var names []string
		for _, d := range c.Deps {
			if d.Manager == "npm" {
				names = append(names, d.Name)
			}
		}
		if len(names) > 0 {
			out = append(out, scStep{
				ID: "npm", Label: strings.Join(names, ", "), Kind: "dep",
				// `npm ls` and not "is there a node_modules/<pkg> directory": the
				// directory check cannot see a *version* change, so tightening a
				// dependency in the registry left the old copy installed and the sample
				// still failing. (Exactly that: the MongoDB driver was pinned down from 7
				// to 6 for the Node the base images ship, and the pin did nothing.)
				// npm ls compares the installed tree against package.json and exits
				// non-zero when they disagree, in under a second.
				Check: "npm ls --depth=0 >/dev/null 2>&1",
				Cmd:   "set -e\nnpm install --no-audit --no-fund",
				Show:  "npm install", Dir: g.Dir, Env: env,
			})
		}
	case scRuntimeGo:
		// Go is the one step here with no check, and the reason is worth stating: every
		// other check asks a cheap true question whose answer survives a re-save — is the
		// binary there, is the package in node_modules, is there a target/classes. Go's
		// cannot, because DBCanvas rewrites go.mod every time the project is saved and
		// that drops the indirect requires `go mod tidy` had added. Two checks were tried
		// against a live node and both were wrong: "does go.sum exist" let a stale go.sum
		// through (`go run .` then failed with "updates to go.mod needed"), and
		// `go list -mod=readonly ./...` answered "fine" on a project with no go.sum at
		// all, because without -deps it never looks at the imports.
		//
		// So the resolve just runs, which is honest rather than wasteful: `go mod tidy`
		// against a warm module cache is under a second and downloads nothing. The cache
		// lives under $HOME/go and survives a Reset, so the second sample needing the same
		// driver reads it from disk.
		out = append(out, scStep{
			ID: "gomod", Label: "Go modules", Kind: "dep",
			Cmd:  "set -e\ngo mod tidy",
			Show: "go mod tidy", Dir: g.Dir,
			Env: append([]string{"GOTOOLCHAIN=auto", "HOME=/root"}, env...),
		})
	case scRuntimeJava:
		out = append(out, scStep{
			ID: "maven", Label: "Maven dependencies", Kind: "dep",
			// Maven's own cache is ~/.m2; target/classes is this project's compiled output and
			// the cheapest true statement that both have happened.
			Check: "[ -d target/classes ]",
			Cmd:   "set -e\nmvn -B -q compile",
			Show:  "mvn -B compile", Dir: g.Dir,
			Env: append([]string{"HOME=/root"}, env...),
		})
	}
	return out
}

// scPrepareSteps builds the material a client needs beside the source before it will run.
//
// Only Java needs any, and only for TLS — but which material depends on the driver, not on the
// language, which is why this dispatches on the database too:
//
//   - Connector/J and the MongoDB driver read trust material from a JVM keystore. A PEM CA has to
//     become a PKCS#12 truststore first, which is what keytool is for.
//   - pgJDBC reads a PEM certificate authority directly, so it needs no truststore at all — but
//     it will not read a PEM *private key*, so mutual TLS needs the key converted to PKCS#8 DER.
//
// Every step is checked before it runs, so a second run of the same sample converts nothing.
func scPrepareSteps(c scClient, g scGen) []scStep {
	if c.Runtime != scRuntimeJava {
		return nil
	}
	var out []scStep
	if c.Database == scPostgres {
		if g.MTLS() {
			out = append(out, scStep{
				ID: "pkcs8", Label: "PKCS#8 client key for pgJDBC", Kind: "prepare",
				Check: "[ -f client-key.pk8 ]",
				Cmd: "set -e\nopenssl pkcs8 -topk8 -outform DER -in " + g.ClientKey +
					" -out client-key.pk8 -nocrypt\nchmod 600 client-key.pk8",
				Show: "openssl pkcs8 -topk8 -outform DER -in client-key.pem -out client-key.pk8 -nocrypt",
				Dir:  g.Dir,
			})
		}
		return out
	}
	if g.CA != "" {
		out = append(out, scStep{
			ID: "truststore", Label: "PKCS#12 truststore from the DBCanvas CA", Kind: "prepare",
			Check: "[ -f truststore.p12 ]",
			Cmd: "set -e\nkeytool -importcert -noprompt -alias dbcanvas -file " + g.CA +
				" -keystore truststore.p12 -storetype PKCS12 -storepass " + scStorePass,
			Show: "keytool -importcert -alias dbcanvas -file " + g.CA + " -keystore truststore.p12 -storetype PKCS12",
			Dir:  g.Dir,
		})
	}
	if g.MTLS() {
		out = append(out, scStep{
			ID: "keystore", Label: "PKCS#12 keystore from the client certificate", Kind: "prepare",
			Check: "[ -f keystore.p12 ]",
			Cmd: "set -e\nopenssl pkcs12 -export -in " + g.ClientCert + " -inkey " + g.ClientKey +
				" -name dbcanvas -out keystore.p12 -passout pass:" + scStorePass,
			Show: "openssl pkcs12 -export -in client-cert.pem -inkey client-key.pem -out keystore.p12",
			Dir:  g.Dir,
		})
	}
	return out
}

// scResetScript empties one project directory, leaving the shared caches — the virtualenv, the Go
// module cache, ~/.m2 — alone. "Start again" almost never means "download the internet again".
func scResetScript(dir string) string {
	return fmt.Sprintf(`set -e
case "%s" in %s/*) ;; *) echo "refusing to remove a directory outside %s"; exit 1;; esac
rm -rf %s`, dir, scRoot, scRoot, dir)
}
