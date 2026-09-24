package main

import (
	"fmt"
	"strconv"
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
	scRuntimeDotnet = "dotnet"
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
// scOS is the node's distribution *and* its release. The release is not decoration: one
// "oraclelinux" covers EL8, which pins Python 3.6 and Node 10 behind module streams, and EL10,
// which ships neither problem. Every version-dependent decision below reads this rather than
// guessing from the family name.
type scOS struct {
	ID      string // oraclelinux | rocky | almalinux | centos | debian | ubuntu
	Version string // "7", "8", "9", "10", "22.04", "24.04", "12", "13"
}

// Debian reports whether this is an apt distribution.
func (o scOS) Debian() bool { return isDebianOS(o.ID) }

// Major is the leading integer of the release ("22.04" is 22, "8" is 8), or 0 when the release
// was not recorded — in which case every version test below answers "no special case", which is
// the right default for a node whose version DBCanvas does not know.
func (o scOS) Major() int {
	n := 0
	for _, r := range o.Version {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// EL reports whether this is Enterprise Linux at exactly this major release.
func (o scOS) EL(major int) bool { return !o.Debian() && o.Major() == major }

// EL7 reports whether this is CentOS 7, the end-of-life release offered with EOL=on
// (linuxclient_el7.go). It is its own predicate rather than EL(7) because it is true from the family
// alone — the only CentOS DBCanvas offers is 7, and some callers have only the family to go on.
//
// Almost nothing about it is the EL8+ story. There is no dnf and no module streams; glibc is 2.17,
// which no current Node.js or .NET build runs on; python3 is 3.6 and the JDK stops at 11. What does
// run there is listed per package below, and every answer in it was found on a live node.
func (o scOS) EL7() bool { return isEL7OS(o.ID) }

// Is reports whether this is the named distribution at exactly this release.
func (o scOS) Is(id, version string) bool { return o.ID == id && o.Version == version }

// scModules is the EL module streams to change before installing.
//
// EL8 is the only release that needs this, and it needs it for two unrelated reasons. A default
// stream *filters the repository*: with the `mysql` module at its default, dnf reports Percona's
// own percona-server-client as "no matching package" even though it is right there in an enabled
// repository ("All matches were filtered out by modular filtering"). And a default stream *pins a
// runtime*: nodejs:10 and python36 are what EL8 offers until something says otherwise, and both
// are too old for the current drivers.
type scModules struct {
	Disable []string // streams to get out of the way of a third-party package of the same name
	Enable  []string // "name:stream" to switch to, replacing whatever is enabled now
}

// scTarball is an upstream binary release installed straight from the project that publishes
// it, for the one case the distribution cannot answer: a release whose archive has no build of a
// runtime new enough to compile the current drivers, and no versioned package either. Ubuntu
// 22.04 carries Node 12 and nothing newer; Debian 12 carries Go 1.19 and nothing newer, backports
// included. Both are below what the drivers require, and neither is going to change.
//
// This is a download, not a repository: no third-party apt or yum source is added to the node,
// nothing outside the named archive is installed, and the URL is the project's own. The version
// is pinned and the SHA-256 of each architecture's archive is pinned beside it, so the install
// either produces exactly the reviewed bytes or fails — the same standard scDep sets for the
// libraries a sample resolves, applied to the runtime under it.
type scTarball struct {
	Name    string            // what is being installed, for the log
	Version string            // pinned
	URL     string            // %s is replaced by the archive's architecture token
	Arch    map[string]string // uname -m -> the token this project uses in its filenames
	SHA256  map[string]string // uname -m -> the archive's checksum
	Dir     string            // where it unpacks to
	Strip   int               // tar --strip-components
	Bins    []string          // binaries to link into /usr/local/bin, which is already on PATH
	// BinDir is where Bins are, relative to Dir: "bin" when empty. The .NET SDK archive keeps
	// its `dotnet` at the top of the tree, which is "." here.
	BinDir  string
	License string
}

type scSysPkg struct {
	ID    string
	Label string
	// Check is a shell test: exit 0 means the package is already usable. It tests the *tool*,
	// not the package database, because "the rpm is installed" and "the command runs" are
	// different statements and only the second one matters here.
	Check func(os scOS, t scTarget) string
	// Packages is what to install, per OS family and release.
	Packages func(os scOS, t scTarget) []string
	// Alt is a second list to try when Packages fails, for a package name that genuinely differs
	// between releases of the same family (the JDK on EL8 vs EL9+).
	Alt func(os scOS, t scTarget) []string
	// Modules are the EL module streams to change first. Nil everywhere but EL8.
	Modules func(os scOS) scModules
	// Tarball is the upstream release to install instead, on the releases whose own archive
	// has nothing new enough. Nil everywhere else, which is almost everywhere.
	Tarball func(os scOS) *scTarball
	// Repo is the percona-release product to enable before installing, or "" for the distro's
	// own repositories. It takes the OS because on CentOS 7 the answer is not the target's series:
	// Percona stopped building for el7 before most of the current ones.
	Repo func(os scOS, t scTarget) string
	// Unsupported is why this cannot be installed on a release at all, or "" when it can. A
	// client that needs something unsupported is refused before anything is installed
	// (scUnsupported), rather than failing halfway through its environment.
	Unsupported func(os scOS) string
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
		//
		// The interpreter tested is the one the virtualenv will be built from, which on EL8 is
		// not `python3`: that name is Python 3.6 there, and a current driver wheel refuses to
		// import on it ("future feature annotations is not defined"). scPythonBin picks it.
		Check: func(os scOS, _ scTarget) string {
			return scPythonBin(os) + ` -c "import venv, ensurepip"`
		},
		Packages: func(os scOS, _ scTarget) []string {
			if os.Debian() {
				return []string{"python3", "python3-pip", "python3-venv"}
			}
			if os.EL(8) {
				// EL8's python3 is 3.6 and its python36 module stream is what holds it there.
				// 3.11 is a separate package rather than a stream, so it installs beside it.
				return []string{"python3.11", "python3.11-pip"}
			}
			if os.EL7() {
				// CentOS 7's python3 is 3.6 too, with nothing newer in base. Software
				// Collections' rh-python38 is the newest Python the release ever got, from the
				// SCL "rh" repository the node is pointed at when it is deployed. Its interpreter
				// carries its own library path, so it runs without `scl enable`.
				return []string{"rh-python38-python", "rh-python38-python-pip"}
			}
			return []string{"python3", "python3-pip"}
		},
		License: "PSF-2.0", URL: "https://www.python.org/",
	},
	"nodejs": {
		ID: "nodejs", Label: "Node.js (with npm)",
		// Node has to be new enough to *parse* the drivers, which is a stricter test than
		// "node exists". mysql2, pg and the MongoDB driver all use optional chaining, so on
		// Node 10 (EL8) and Node 12 (Ubuntu 22.04) they fail at require time with
		// "SyntaxError: Unexpected token ." — after npm installed them without complaint,
		// because their package.json engines field still says >= 8. Asking the interpreter to
		// parse the syntax is the honest check.
		Check: func(scOS, scTarget) string {
			return `command -v node >/dev/null && command -v npm >/dev/null && node -e "const o={};o?.x" >/dev/null 2>&1`
		},
		Packages: func(scOS, scTarget) []string { return []string{"nodejs", "npm"} },
		// EL8 defaults to nodejs:10 and stays there until the stream is switched.
		Modules: func(os scOS) scModules {
			if os.EL(8) {
				return scModules{Enable: []string{"nodejs:20"}}
			}
			return scModules{}
		},
		// Ubuntu 22.04's archive has Node 12 and nothing else, on any channel. CentOS 7 has no
		// Node at all, and the official builds need glibc 2.28 — it gets the Node.js project's
		// glibc 2.17 build of the same release.
		Tarball: func(os scOS) *scTarball {
			if os.Is("ubuntu", "22.04") {
				return &scNodeUpstream
			}
			if os.EL7() {
				return &scNodeUpstreamEL7
			}
			return nil
		},
		License: "MIT", URL: "https://nodejs.org/",
	},
	"golang": {
		ID: "golang", Label: "Go toolchain",
		// The drivers' own go.mod files require Go 1.24, so "go exists" is not the question —
		// go-sql-driver/mysql declares `go 1.24.0` and an older toolchain refuses the module
		// outright ("go.mod file indicates go 1.21, but maximum version supported by tidy is
		// 1.19"). GOTOOLCHAIN would paper over it from 1.21 onwards, but it needs to reach
		// proxy.golang.org to do so and fails closed on a lab node that cannot
		// ("toolchain not available"), so the toolchain on disk has to be new enough itself.
		Check: func(scOS, scTarget) string { return scGoAtLeast(scGoMinMajor, scGoMinMinor) },
		Packages: func(os scOS, _ scTarget) []string {
			if os.Debian() {
				// Debian and Ubuntu both carry versioned golang-1.NN metapackages beside the
				// unversioned one, and on the releases whose golang-go is too old that is the
				// only way to a current toolchain from the distribution's own archive.
				if v := scDebianGoPackage(os); v != "" {
					return []string{v}
				}
				return []string{"golang-go"}
			}
			return []string{"golang"}
		},
		Alt: func(os scOS, _ scTarget) []string {
			if os.Debian() {
				return []string{"golang-go"}
			}
			return nil
		},
		// Debian 12 has golang-go 1.19, no versioned golang-1.2x packages, and no newer Go in
		// backports either — the only route to a toolchain the drivers accept is upstream's.
		// CentOS 7 has no Go at all. The upstream toolchain is statically linked, so the same
		// archive runs on its glibc 2.17.
		Tarball: func(os scOS) *scTarball {
			if os.Is("debian", "12") || os.EL7() {
				return &scGoUpstream
			}
			return nil
		},
		License: "BSD-3-Clause", URL: "https://go.dev/",
	},
	"jdk": {
		ID: "jdk", Label: "JDK (OpenJDK)",
		// The generated pom compiles at release 17, so the check asks javac whether it can do
		// that rather than whether it exists. `command -v javac` passes on the JDK 11 that
		// Ubuntu 22.04's default-jdk and EL8's default both install, and the build then dies
		// much later with "release version 17 not supported".
		Check: func(scOS, scTarget) string { return scJavaCapable },
		Packages: func(os scOS, _ scTarget) []string {
			if os.Debian() {
				// default-jdk is 11 on Ubuntu 22.04, which is below the release level above.
				// Naming the version wanted is what makes this right on every release.
				return []string{"openjdk-21-jdk"}
			}
			return []string{"java-21-openjdk-devel"}
		},
		// EL8 has no java-21 package, and Debian 12 has no openjdk-21. 17 is the LTS both
		// ship, and it is exactly the release level the pom asks for. Tried only if the first
		// list fails, so a node that has 21 gets 21.
		Alt: func(os scOS, _ scTarget) []string {
			if os.Debian() {
				return []string{"openjdk-17-jdk"}
			}
			return []string{"java-17-openjdk-devel"}
		},
		// CentOS 7 stops at java-11-openjdk, below the release level the pom compiles at.
		// Eclipse Temurin still builds against glibc 2.17, and unpacks under /usr/lib/jvm so
		// scJavaCapable and scJavaHome find it the way they find a packaged JDK.
		Tarball: func(os scOS) *scTarball {
			if os.EL7() {
				return &scJDKUpstreamEL7
			}
			return nil
		},
		License: "GPL-2.0-only WITH Classpath-exception-2.0", URL: "https://openjdk.org/",
	},
	"maven": {
		ID: "maven", Label: "Apache Maven",
		// Whatever the distribution ships. EL8's is 3.5.4 and there is no moving it: the
		// maven:3.8 stream exists, but its packages symlink the jars in /usr/share/maven/lib
		// into the maven-resolver rpm from the 3.5 stream, which no module switch replaces —
		// Maven then starts and dies on a NoSuchMethodError inside its own resolver. The
		// generated pom pins plugins that run on 3.5 instead, which costs nothing (the release
		// level decides the bytecode, not the plugin version) and works on every node.
		Check:    func(scOS, scTarget) string { return `command -v mvn >/dev/null` },
		Packages: func(scOS, scTarget) []string { return []string{"maven"} },
		// CentOS 7's is 3.0.5, which is below even what the pom's plugins pin for EL8:
		// exec-maven-plugin 3.1.0 declares Maven 3.2.5 as its minimum. Apache's own binary
		// archive is pure Java and runs on any JDK the step above installed.
		Tarball: func(os scOS) *scTarball {
			if os.EL7() {
				return &scMavenUpstream
			}
			return nil
		},
		License: "Apache-2.0", URL: "https://maven.apache.org/",
	},
	"icu": {
		ID: "icu", Label: "ICU (International Components for Unicode)",
		// .NET's globalization calls into libicu at startup and exits with "Couldn't find a
		// valid ICU package" without it. The distributions' SDK packages depend on it already;
		// the upstream archive installed on Debian cannot, so it is asked for here, by the
		// soname-versioned name each release gives it.
		Check: func(scOS, scTarget) string { return `ldconfig -p | grep -q 'libicuuc\.so'` },
		Packages: func(os scOS, _ scTarget) []string {
			if !os.Debian() {
				return []string{"libicu"}
			}
			switch {
			case os.Is("ubuntu", "22.04"):
				return []string{"libicu70"}
			case os.Is("ubuntu", "24.04"):
				return []string{"libicu74"}
			case os.Is("debian", "12"):
				return []string{"libicu72"}
			}
			return []string{"libicu76"} // Debian 13, and the guess for a release newer than this list
		},
		Unsupported: scNoDotnetOnEL7,
		License:     "Unicode-3.0", URL: "https://icu.unicode.org/",
	},
	"dotnet-sdk": {
		ID: "dotnet-sdk", Label: ".NET SDK",
		// The generated project targets net8.0 and rolls forward to whatever newer runtime is
		// installed, so any SDK from 8 up builds and runs it. The check asks the SDK list, not
		// whether `dotnet` exists: a node with only a runtime has the command and cannot build.
		Check: func(scOS, scTarget) string {
			return `command -v dotnet >/dev/null && dotnet --list-sdks 2>/dev/null | awk -F. '$1+0>=` +
				strconv.Itoa(scDotnetMinSDK) + `{f=1} END{exit !f}'`
		},
		Packages: func(os scOS, _ scTarget) []string {
			// Ubuntu 22.04 carries .NET 6, 7 and 8 and nothing newer, so asking it for 10
			// would only be a guaranteed failure before the fallback.
			if os.Is("ubuntu", "22.04") {
				return []string{"dotnet-sdk-8.0"}
			}
			// .NET 10 is the current LTS, and Oracle Linux 8, 9 and 10 and Ubuntu 24.04 all
			// ship it from their own AppStream or archive.
			return []string{"dotnet-sdk-10.0"}
		},
		Alt: func(scOS, scTarget) []string { return []string{"dotnet-sdk-8.0"} },
		// Debian packages no .NET at all, in any release or in backports. Microsoft's own apt
		// repository would be a third-party source on the node; the SDK archive is not.
		Tarball: func(os scOS) *scTarball {
			if os.ID == "debian" {
				return &scDotnetUpstream
			}
			return nil
		},
		Unsupported: scNoDotnetOnEL7,
		License:     "MIT", URL: "https://dotnet.microsoft.com/",
	},
	"openssl": {
		ID: "openssl", Label: "OpenSSL command line",
		Check:    func(scOS, scTarget) string { return `command -v openssl >/dev/null` },
		Packages: func(scOS, scTarget) []string { return []string{"openssl"} },
		License:  "Apache-2.0", URL: "https://www.openssl.org/",
	},
	"mysql-client": {
		ID: "mysql-client", Label: "mysql (Percona Server client)",
		Check: func(scOS, scTarget) string { return `command -v mysql >/dev/null` },
		Packages: func(os scOS, t scTarget) []string {
			// On CentOS 7 the 5.7 client is its own package name, as it is in the 5.7 repo
			// everywhere; every newer target gets the 8.0 client (see Repo).
			if os.EL7() && psMajorOf(scMajorOr(t.Major, "8.0")) == "5.7" {
				return []string{"Percona-Server-client-57"}
			}
			return []string{"percona-server-client"}
		},
		// Percona had not published this repository for Debian 13 (trixie) when this was
		// written, and apt says only "Unable to locate package". mariadb-client speaks the
		// same wire protocol and installs the same `mysql` command, which is what the sample
		// actually invokes.
		Alt: func(os scOS, _ scTarget) []string {
			if os.Debian() {
				return []string{"mariadb-client"}
			}
			return nil
		},
		// EL8's mysql module hides percona-server-client behind modular filtering.
		Modules: func(os scOS) scModules {
			if os.EL(8) {
				return scModules{Disable: []string{"mysql"}}
			}
			return scModules{}
		},
		// Same repository the ProxySQL node uses for the same binary — the series the target
		// runs, so the client is never older than the server it is pointed at. Except on
		// CentOS 7, where Percona's last el7 client is 8.0 (8.0.37): there is no ps-84-lts or 9.x
		// build for el7, and an 8.0 client speaks to both.
		Repo: func(os scOS, t scTarget) string {
			m := psMajorOf(scMajorOr(t.Major, "8.0"))
			if os.EL7() && m != "5.7" {
				m = "8.0"
			}
			return psClientProduct(m)
		},
		License: "GPL-2.0-only", URL: "https://www.percona.com/mysql",
	},
	"psql-client": {
		ID: "psql-client", Label: "psql (Percona Distribution for PostgreSQL client)",
		// On EL the client lands under /usr/pgsql-NN/bin and is not on PATH; on Debian it is.
		// Both are accepted, and the generated script resolves it the same way.
		Check: func(os scOS, t scTarget) string {
			return `command -v psql >/dev/null || [ -x ` + scPgBinDir(os.ID, t) + `/psql ]`
		},
		Packages: func(os scOS, t scTarget) []string {
			m := scPgClientMajor(os.ID, t)
			if os.Debian() {
				return []string{"percona-postgresql-client-" + m}
			}
			return []string{"percona-postgresql" + m}
		},
		// Same modular filtering as the MySQL client, under EL8's postgresql module.
		Modules: func(os scOS) scModules {
			if os.EL(8) {
				return scModules{Disable: []string{"postgresql"}}
			}
			return scModules{}
		},
		Repo:    func(os scOS, t scTarget) string { return ppgProduct(scPgClientMajor(os.ID, t)) },
		License: "PostgreSQL", URL: "https://www.percona.com/postgresql",
	},
	"mongosh": {
		ID: "mongosh", Label: "mongosh (MongoDB Shell)",
		Check:    func(scOS, scTarget) string { return `command -v mongosh >/dev/null` },
		Packages: func(scOS, scTarget) []string { return []string{"percona-mongodb-mongosh"} },
		// psmdb-80 has no el7 build; psmdb-60 and -70 both carry mongosh 2.1.5 for it, and a
		// mongosh is not tied to the server's series the way a mysql client is.
		Repo: func(os scOS, t scTarget) string {
			if os.EL7() {
				return "psmdb-70"
			}
			return psmdbRepo(scMajorOr(t.Major, "8.0"))
		},
		License: "Apache-2.0", URL: "https://github.com/mongodb-js/mongosh",
	},
	"valkey-cli": {
		ID: "valkey-cli", Label: "valkey-cli",
		Check: func(scOS, scTarget) string { return `command -v valkey-cli >/dev/null` },
		Packages: func(os scOS, _ scTarget) []string {
			// Debian splits the CLI tools out of the server package; EL bundles them.
			// Same split valkeyPackages documents for the Valkey node itself.
			if os.Debian() {
				return []string{"percona-valkey-tools"}
			}
			return []string{"percona-valkey"}
		},
		Repo: func(scOS, scTarget) string { return "valkey-91" },
		Unsupported: func(os scOS) string {
			if os.EL7() {
				return "Percona publishes no Valkey build for CentOS 7, so there is no valkey-cli to install"
			}
			return ""
		},
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
	case scRuntimeDotnet:
		return []string{"icu", "dotnet-sdk"}
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
// scGoUpstream and scNodeUpstream are the pinned upstream releases installed on the two
// distributions whose own archives cannot reach the minimum. Versions and checksums were taken
// from each project's own published index (go.dev/dl/?mode=json, nodejs.org/dist/SHASUMS256.txt).
//
// Go is licensed BSD-3-Clause and Node.js MIT; DBCanvas installs them here and redistributes
// neither, exactly as it treats the libraries in scDep.
var scGoUpstream = scTarball{
	Name: "Go", Version: "1.27.1",
	URL:  "https://go.dev/dl/go1.27.1.linux-%s.tar.gz",
	Arch: map[string]string{"x86_64": "amd64", "aarch64": "arm64"},
	SHA256: map[string]string{
		"x86_64":  "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445",
		"aarch64": "3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec",
	},
	Dir: "/usr/local/go", Strip: 1, Bins: []string{"go", "gofmt"},
	License: "BSD-3-Clause",
}

// Node 22 is an active LTS and the same major Oracle Linux 10 ships, so a sample behaves the
// same on both rather than differing by which node it happened to run on.
var scNodeUpstream = scTarball{
	Name: "Node.js", Version: "22.23.2",
	URL:  "https://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-%s.tar.xz",
	Arch: map[string]string{"x86_64": "x64", "aarch64": "arm64"},
	SHA256: map[string]string{
		"x86_64":  "d60acfe00a2932254bb0ad20e01b0d74397a0875595de719654b214f4b03f307",
		"aarch64": "fff4078c5def658577f92c88db7db3bc0072924bfb93fe52c1e744a54e94abb8",
	},
	Dir: "/usr/local/node", Strip: 1, Bins: []string{"node", "npm", "npx"},
	License: "MIT",
}

// scDotnetUpstream is the .NET SDK for the one family that packages none, Debian. The version and
// the checksums are from Microsoft's own release index
// (builds.dotnet.microsoft.com/dotnet/release-metadata/10.0/releases.json). The archive holds
// the SDK and the runtime it ships with, so nothing else is fetched to run a program.
//
// The .NET SDK is MIT-licensed; DBCanvas installs it here and redistributes nothing.
var scDotnetUpstream = scTarball{
	Name: ".NET SDK", Version: "10.0.401",
	URL:  "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.401/dotnet-sdk-10.0.401-linux-%s.tar.gz",
	Arch: map[string]string{"x86_64": "x64", "aarch64": "arm64"},
	SHA256: map[string]string{
		"x86_64":  "137268c8ad939c064ff1ee2a6fdf0899d8725377114ea012fbd1ad5fa2550418",
		"aarch64": "91d3d67f2ed065909bd2e1ba50a37192aff6df15178304939d5b55634d72da6a",
	},
	Dir: "/usr/local/dotnet", Strip: 0, Bins: []string{"dotnet"}, BinDir: ".",
	License: "MIT",
}

// scNodeUpstreamEL7 is the same Node.js release as scNodeUpstream, built by the Node.js project
// against glibc 2.17 (unofficial-builds.nodejs.org — the project's own build infrastructure for the
// platforms its release builds dropped). The official Node 18+ archives need glibc 2.28, which is
// why CentOS 7 cannot use scNodeUpstream. There is no aarch64 build of this flavour, so an arm64
// CentOS 7 node is refused by the install script rather than handed the wrong binary.
// Checksum from the release's own SHASUMS256.txt.
var scNodeUpstreamEL7 = scTarball{
	Name: "Node.js (glibc 2.17 build)", Version: "22.23.2",
	URL:  "https://unofficial-builds.nodejs.org/download/release/v22.23.2/node-v22.23.2-linux-%s-glibc-217.tar.xz",
	Arch: map[string]string{"x86_64": "x64"},
	SHA256: map[string]string{
		"x86_64": "333a2c084cbeb3fc8cc026b7cc8eb194cec31803142a0c56e58845eafbfe4495",
	},
	Dir: "/usr/local/node", Strip: 1, Bins: []string{"node", "npm", "npx"},
	License: "MIT",
}

// scJDKUpstreamEL7 is Eclipse Temurin 21 for CentOS 7, whose own archive stops at JDK 11. Version
// and checksums are Adoptium's (api.adoptium.net/v3/assets/latest/21/hotspot). It unpacks under
// /usr/lib/jvm, where scJavaCapable and scJavaHome look for a JDK, so nothing downstream treats it
// differently from a packaged one.
//
// OpenJDK is GPL-2.0 with the Classpath Exception; DBCanvas installs it and redistributes nothing.
var scJDKUpstreamEL7 = scTarball{
	Name: "Eclipse Temurin JDK", Version: "21.0.12.1+1",
	URL:  "https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.12.1%%2B1/OpenJDK21U-jdk_%s_linux_hotspot_21.0.12.1_1.tar.gz",
	Arch: map[string]string{"x86_64": "x64", "aarch64": "aarch64"},
	SHA256: map[string]string{
		"x86_64":  "ce79869e1307ed8ee1e2baa86a412b1eb5b75d10a01006d788a6f968bcfaee94",
		"aarch64": "23e37e026f12f3e706f18938ff611db3032d075b09d0879a25d06718c773e223",
	},
	Dir: "/usr/lib/jvm/temurin-21", Strip: 1, Bins: []string{"java", "javac", "keytool"},
	License: "GPL-2.0-only WITH Classpath-exception-2.0",
}

// scMavenUpstream is Apache Maven's binary archive, for CentOS 7 (whose maven is 3.0.5). Pure Java,
// so one archive serves every architecture. The SHA-256 was taken after the download matched the
// SHA-512 Apache publishes beside it.
var scMavenUpstream = scTarball{
	Name: "Apache Maven", Version: "3.9.16",
	URL:  "https://archive.apache.org/dist/maven/maven-3/3.9.16/binaries/apache-maven-3.9.16-bin.tar.gz",
	Arch: map[string]string{"x86_64": "", "aarch64": ""},
	SHA256: map[string]string{
		"x86_64":  "80ffca22aed9e8b9713a232f3394fd81d7f20322df75efdb2b047dbd3e3a23bb",
		"aarch64": "80ffca22aed9e8b9713a232f3394fd81d7f20322df75efdb2b047dbd3e3a23bb",
	},
	Dir: "/usr/local/maven", Strip: 1, Bins: []string{"mvn"},
	License: "Apache-2.0",
}

// scDotnetMinSDK is the oldest SDK major that builds the generated project, which targets net8.0.
const scDotnetMinSDK = 8

// scDotnetProject is the project file every C# sample is built from.
const scDotnetProject = "DbCanvasSample.csproj"

// scDotnetEnv is what every dotnet step runs with. HOME puts the NuGet cache at
// /root/.nuget/packages, shared by every C# sample on the node; the rest turns off the first-run
// banner and the telemetry notice, which would otherwise be the first twenty lines of the log.
var scDotnetEnv = []string{
	"HOME=/root", "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1",
	"DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1", "DOTNET_GENERATE_ASPNET_CERTIFICATE=false",
}

// scGoMinMajor/scGoMinMinor is the Go the generated projects need, which is set by the drivers
// rather than by the samples: go-sql-driver/mysql declares `go 1.24.0` in its own go.mod, and a
// module cannot be built by a toolchain older than the one it requires.
const (
	scGoMinMajor = 1
	scGoMinMinor = 24
)

// scJavaRelease is the --release the generated pom compiles at, and so the minimum JDK.
const scJavaRelease = "17"

// scJavaCapable is a shell test for "a JDK that can compile at scJavaRelease exists here".
//
// It looks past $PATH deliberately. The OpenJDK RPMs register their javac through alternatives
// at priority 1, so installing java-21 on EL8 leaves /usr/bin/javac pointing at the java-11 that
// was already there — the JDK is present and the naive check still fails.
const scJavaCapable = `for _j in /usr/lib/jvm/*/bin/javac; do ` +
	`[ -x "$_j" ] && "$_j" --release ` + scJavaRelease + ` -version >/dev/null 2>&1 && exit 0; done; ` +
	`command -v javac >/dev/null && javac --release ` + scJavaRelease + ` -version >/dev/null 2>&1`

// scJavaHome is the prelude every Java step runs: it points JAVA_HOME and $PATH at the newest
// installed JDK that can compile at scJavaRelease, which is what Maven reads to choose a
// compiler. Without it a node with both java-11 and java-21 builds with whichever one
// alternatives happens to favour, and on EL8 that is the one that cannot.
const scJavaHome = `for _j in $(ls -d /usr/lib/jvm/*/bin/javac 2>/dev/null | sort -V); do
  "$_j" --release ` + scJavaRelease + ` -version >/dev/null 2>&1 && JAVA_HOME="${_j%/bin/javac}"
done
if [ -n "$JAVA_HOME" ]; then export JAVA_HOME; PATH="$JAVA_HOME/bin:$PATH"; export PATH; fi
`

// scTarballEnv is the archive's description as the install script reads it. The architecture is
// resolved on the node rather than here, because the app and the Linux Client it is installing on
// are not always the same one — a stack on an aarch64 host runs x86_64 nodes under emulation.
func scTarballEnv(tb *scTarball) []string {
	var url, sum string
	for uname, token := range tb.Arch {
		url += uname + "=" + tb.urlFor(token) + " "
		sum += uname + "=" + tb.SHA256[uname] + " "
	}
	return []string{
		"TB_NAME=" + tb.Name, "TB_VERSION=" + tb.Version,
		"TB_URLS=" + strings.TrimSpace(url), "TB_SHA256S=" + strings.TrimSpace(sum),
		"TB_DIR=" + tb.Dir, "TB_STRIP=" + strconv.Itoa(tb.Strip),
		"TB_BINS=" + strings.Join(tb.Bins, " "), "TB_BINDIR=" + tb.binDir(),
	}
}

// urlFor is the archive's URL for one architecture token. An archive with no %s in its URL is the
// same file everywhere (Maven is pure Java), and Sprintf would append "%!(EXTRA …)" to it.
func (tb *scTarball) urlFor(token string) string {
	if !strings.Contains(strings.ReplaceAll(tb.URL, "%%", ""), "%s") {
		return strings.ReplaceAll(tb.URL, "%%", "%")
	}
	return fmt.Sprintf(tb.URL, token)
}

func (tb *scTarball) binDir() string {
	if tb.BinDir == "" {
		return "bin"
	}
	return tb.BinDir
}

// scGoAtLeast is a shell test for the Go toolchain's own version. `go version` prints
// "go version go1.22.2 linux/amd64"; the third field without its "go" prefix is what is compared,
// numerically and field by field, so 1.9 does not read as newer than 1.24.
func scGoAtLeast(major, minor int) string {
	return fmt.Sprintf(`command -v go >/dev/null && go version 2>/dev/null | `+
		`awk '{sub(/^go/,"",$3); split($3,v,"."); exit !(v[1]>%d || (v[1]==%d && v[2]>=%d))}'`,
		major, major, minor)
}

// scDebianGoPackage is the versioned Go metapackage to ask for on a Debian or Ubuntu release
// whose unversioned golang-go is older than the drivers need, or "" when golang-go is fine.
//
// The versioned packages install outside $PATH (/usr/lib/go-1.24/bin), which scGoPath puts back.
func scDebianGoPackage(os scOS) string {
	switch {
	case os.Is("ubuntu", "22.04"), // golang-go is 1.18
		os.Is("ubuntu", "24.04"): // golang-go is 1.22
		return "golang-1.24"
	}
	return ""
}

// scGoPath is PATH with the versioned Go's bin directory in front, for the releases where that
// is where the current toolchain lives. Harmless where the directory does not exist.
func scGoPath(os scOS) string {
	if v := scDebianGoPackage(os); v != "" {
		return "/usr/lib/go-" + strings.TrimPrefix(v, "golang-") + "/bin:/usr/local/bin:/usr/bin:/bin"
	}
	return ""
}

// scPythonBin is the interpreter a virtualenv is built from. EL8's `python3` is 3.6, which is
// below what the current driver wheels support, so 3.11 is installed and named explicitly there.
func scPythonBin(os scOS) string {
	if os.EL(8) {
		return "python3.11"
	}
	if os.EL7() {
		return scPythonEL7
	}
	return "python3"
}

// scPythonEL7 is rh-python38's interpreter. By full path, because Software Collections install
// under /opt/rh and put nothing on PATH.
const scPythonEL7 = "/opt/rh/rh-python38/root/usr/bin/python3.8"

func scPgBinDir(nodeOS string, t scTarget) string {
	return pgBinDir(nodeOS, scPgClientMajor(nodeOS, t))
}

// scPgClientMajor is the PostgreSQL series whose psql is installed for a target: the target's own
// everywhere but CentOS 7. Percona's el7 builds go up to 16, but 14, 15 and 16 all require libzstd,
// which CentOS 7 never shipped (it was an EPEL package) — yum refuses them with "Requires: libzstd".
// 13 is the newest that installs from the node's own repositories, and a psql 13 speaks the same
// protocol and SCRAM authentication to 17 and 18; only its \d-style introspection can lag, which
// the samples do not use.
func scPgClientMajor(nodeOS string, t scTarget) string {
	m := ppgMajorOf(scMajorOr(t.Major, "17"))
	if isEL7OS(nodeOS) {
		if n, err := strconv.Atoi(m); err == nil && n > scPgEL7Max {
			return strconv.Itoa(scPgEL7Max)
		}
	}
	return m
}

// scPgEL7Max is the newest Percona Distribution for PostgreSQL whose el7 psql installs without EPEL.
const scPgEL7Max = 13

// scNoDotnetOnEL7 is the one reason C# cannot run on CentOS 7, found by running both SDKs there:
// the .NET 8 and 10 archives both need GLIBCXX_3.4.21 from libstdc++, and CentOS 7 has 3.4.19.
// Shared by the SDK and ICU so a C# client names it once, whichever it checks first.
func scNoDotnetOnEL7(os scOS) string {
	if os.EL7() {
		return ".NET 8 and later need a newer libstdc++ (GLIBCXX_3.4.21) than CentOS 7 has, so no .NET SDK runs there"
	}
	return ""
}

// scUnsupportedOn is every client that cannot run on a release, keyed database/language/client —
// the catalogue's own client id (language/client) under its database, because that id alone is not
// unique (go/database-sql speaks MySQL and PostgreSQL). Valued with the reason. Empty, not nil, on
// every release but CentOS 7, so the JSON is always an object.
func scUnsupportedOn(os scOS) map[string]string {
	out := map[string]string{}
	for _, c := range scClients {
		if why := scUnsupported(c, os); why != "" {
			out[c.Database+"/"+c.Language+"/"+c.ID] = why
		}
	}
	return out
}

// scUnsupported is why a client cannot run on this node's release at all, or "" when it can: the
// first reason any package it needs gives. Checked before a plan is built, so the answer is one
// sentence rather than an install log that fails at step three.
func scUnsupported(c scClient, os scOS) string {
	for _, id := range append(scRuntimePackages(c.Runtime), c.SysPackages...) {
		if p, ok := scSysPackages[id]; ok && p.Unsupported != nil {
			if why := p.Unsupported(os); why != "" {
				return why
			}
		}
	}
	return ""
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
	case "nuget":
		return "NuGet"
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
for m in $MOD_DISABLE; do
  echo "dnf -y module disable $m"
  dnf -y module disable "$m" || true
done
for m in $MOD_ENABLE; do
  echo "dnf -y module reset ${m%%:*} && dnf -y module enable $m"
  dnf -y module reset "${m%%:*}" || true
  dnf -y module enable "$m" || true
  # Enabling a stream does not move the packages already installed from the old one, and a
  # half-switched module is worse than either: EL8's maven:3.8 arrives with a launcher that
  # expects guava 27 while guava20 from maven:3.5 is still what is installed, leaving
  # /usr/share/maven/lib/guava-27.1-jre.jar a dangling symlink and every build dying on
  # "NoClassDefFoundError: com/google/common/collect/ImmutableList". distro-sync is what
  # Red Hat documents to reconcile the two, and it only runs when a stream actually changed.
  echo "dnf -y distro-sync"
  dnf -y distro-sync || true
done
if ! dnf -y install $PKGS; then
  [ -n "$ALT" ] || exit 1
  echo "falling back to: $ALT"
  dnf -y install $ALT
fi`

// scInstallTarball fetches one upstream release, checks it against the pinned digest, and links
// its binaries into /usr/local/bin — which is ahead of /usr/bin in the default PATH on every base
// image here, so nothing downstream has to know this runtime arrived differently from a package.
//
// The digest is checked before anything is unpacked, and a node whose architecture is not in the
// pinned set is refused rather than silently given the wrong build.
const scInstallTarball = `set -e
if [ -n "$PROXY" ]; then export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"; fi
arch=$(uname -m)
TB_URL=""; TB_SHA256=""
for kv in $TB_URLS; do [ "${kv%%=*}" = "$arch" ] && TB_URL="${kv#*=}"; done
for kv in $TB_SHA256S; do [ "${kv%%=*}" = "$arch" ] && TB_SHA256="${kv#*=}"; done
[ -n "$TB_URL" ] && [ -n "$TB_SHA256" ] || { echo "no pinned $TB_NAME build for $arch"; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading $TB_URL"
curl -fsSL --retry 3 -o "$tmp/archive" "$TB_URL"
echo "$TB_SHA256  $tmp/archive" | sha256sum -c -
rm -rf "$TB_DIR"
mkdir -p "$TB_DIR"
tar -C "$TB_DIR" --strip-components="$TB_STRIP" -xf "$tmp/archive"
for b in $TB_BINS; do ln -sf "$TB_DIR/${TB_BINDIR:-bin}/$b" "/usr/local/bin/$b"; done
echo "installed $TB_NAME $TB_VERSION in $TB_DIR"`

// scInstallEL7 is scInstallRHEL for CentOS 7: yum, and no module streams to change (they arrived
// with EL8). The vault repositories and the SCL one are already configured — the node was pointed
// at them when it was deployed (el7BootstrapScript).
const scInstallEL7 = `set -e
if [ -n "$PROXY" ]; then export http_proxy="$PROXY" https_proxy="$PROXY" HTTP_PROXY="$PROXY" HTTPS_PROXY="$PROXY"; fi
if [ -n "$REPO" ]; then
  percona-release enable "$REPO" >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1 || true
fi
if ! yum -y install $PKGS; then
  [ -n "$ALT" ] || exit 1
  echo "falling back to: $ALT"
  yum -y install $ALT
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
func scBuildPlan(c scClient, g scGen, os scOS, useProxy bool) scPlan {
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
		pkgs := strings.Join(p.Packages(os, g.Target), " ")
		alt := ""
		if p.Alt != nil {
			alt = strings.Join(p.Alt(os, g.Target), " ")
		}
		repo := ""
		if p.Repo != nil {
			repo = p.Repo(os, g.Target)
		}
		var mods scModules
		if p.Modules != nil {
			mods = p.Modules(os)
		}
		var tb *scTarball
		if p.Tarball != nil {
			tb = p.Tarball(os)
		}
		script := scInstallRHEL
		show := "dnf -y install " + pkgs
		if os.Debian() {
			script, show = scInstallDebian, "apt-get install -y "+pkgs
			mods = scModules{} // module streams are an EL idea
		} else if os.EL7() {
			script, show = scInstallEL7, "yum -y install "+pkgs
			mods = scModules{} // and an EL8+ one
		}
		env := []string{"PKGS=" + pkgs, "ALT=" + alt, "REPO=" + repo, "PROXY=" + proxy,
			"MOD_DISABLE=" + strings.Join(mods.Disable, " "),
			"MOD_ENABLE=" + strings.Join(mods.Enable, " ")}
		if tb != nil {
			// The distribution has nothing new enough, so the package manager is not
			// consulted at all — installing its too-old build first would only leave two
			// runtimes on the node and the wrong one on PATH.
			script = scInstallTarball
			show = "curl -fsSL " + tb.urlFor("$(arch)") +
				" | sha256sum -c | tar -C " + tb.Dir + " -x   # " + tb.Name + " " + tb.Version +
				", " + tb.License + ", checksum pinned"
			env = append(scTarballEnv(tb), "PROXY="+proxy)
		}
		if len(mods.Enable) > 0 {
			show = "dnf -y module enable " + strings.Join(mods.Enable, " ") + " && " + show
		}
		if len(mods.Disable) > 0 {
			show = "dnf -y module disable " + strings.Join(mods.Disable, " ") + " && " + show
		}
		if repo != "" {
			show = "percona-release enable " + repo + " && " + show
		}
		plan.System = append(plan.System, scStep{
			ID: "sys:" + id, Label: p.Label, Kind: "system",
			Check: p.Check(os, g.Target), Cmd: script, Show: show, Env: env,
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
	plan.Deps = append(plan.Deps, scDepSteps(c, g, os, env)...)

	// 3. Anything that has to exist beside the source before it will run. Only Java needs this,
	//    and only for TLS: the JVM reads trust material out of a keystore, never a PEM.
	plan.Prepare = scPrepareSteps(c, g)

	// 4. The program itself.
	runEnv := env
	if c.Runtime == scRuntimeGo {
		// `go run` has to find the same toolchain `go mod tidy` used, and on the releases
		// where that is a versioned package it is not the one on the default PATH.
		runEnv = append(append([]string{}, env...), "HOME=/root")
		if p := scGoPath(os); p != "" {
			runEnv = append(runEnv, "PATH="+p)
		}
	}
	if c.Runtime == scRuntimeDotnet {
		runEnv = append(append([]string{}, scDotnetEnv...), env...)
	}
	runCmd := c.Run(g)
	if c.Runtime == scRuntimeJava {
		// Classes compiled at release 17 will not load on an 11 runtime
		// (UnsupportedClassVersionError), so the program runs under the same JDK that
		// compiled it rather than whatever alternatives points at.
		runCmd = scJavaHome + runCmd
	}
	plan.Run = scStep{
		ID: "run", Label: "Run " + c.Label, Kind: "run",
		Cmd: runCmd, Show: c.Run(g), Dir: g.Dir, Env: runEnv,
	}
	return plan
}

// scDepSteps is the per-ecosystem half of the resolver: pip into the shared virtualenv, npm into
// the project's node_modules, `go mod tidy` against the module cache, Maven into ~/.m2. Each one
// is the tool the ecosystem expects a developer to use, and each one is skipped when its own check
// says the work is already done.
func scDepSteps(c scClient, g scGen, os scOS, env []string) []scStep {
	var out []scStep
	switch c.Runtime {
	case scRuntimePython:
		// The interpreter is named rather than assumed: on EL8 `python3` is 3.6 and the
		// virtualenv has to be built from the 3.11 installed beside it, or every wheel
		// resolved into it is the last one that still supported 3.6.
		py := scPythonBin(os)
		out = append(out, scStep{
			ID: "venv", Label: "Python virtualenv", Kind: "dep",
			// Not just "does the virtualenv exist" — one built from the wrong interpreter has
			// to be rebuilt, and a node upgraded from EL8's 3.6 would otherwise keep it.
			Check: "[ -x " + scVenv + "/bin/python ] && " + scVenv + `/bin/python -c "import sys; sys.exit(sys.version_info < (3, 8))"`,
			Cmd:   "set -e\nrm -rf " + scVenv + "\n" + py + " -m venv " + scVenv,
			Show:  py + " -m venv " + scVenv,
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
		goEnv := append([]string{"GOTOOLCHAIN=auto", "HOME=/root"}, env...)
		if p := scGoPath(os); p != "" {
			// A versioned golang-1.NN package installs outside $PATH, so without this the
			// `go` found here is still the distribution's older unversioned one.
			goEnv = append(goEnv, "PATH="+p)
		}
		out = append(out, scStep{
			ID: "gomod", Label: "Go modules", Kind: "dep",
			Cmd:  "set -e\ngo mod tidy",
			Show: "go mod tidy", Dir: g.Dir,
			Env: goEnv,
		})
	case scRuntimeJava:
		out = append(out, scStep{
			ID: "maven", Label: "Maven dependencies", Kind: "dep",
			// Maven's own cache is ~/.m2; target/classes is this project's compiled output and
			// the cheapest true statement that both have happened.
			Check: "[ -d target/classes ]",
			Cmd:   "set -e\n" + scJavaHome + "mvn -B -q compile",
			Show:  "mvn -B compile", Dir: g.Dir,
			Env: append([]string{"HOME=/root"}, env...),
		})
	case scRuntimeDotnet:
		out = append(out, scStep{
			ID: "nuget", Label: "NuGet packages", Kind: "dep",
			// project.assets.json is what a restore writes, and DBCanvas rewrites the project
			// file on every save — so "the restore is newer than the project" is the true
			// statement, and a changed PackageReference is restored again rather than kept at
			// the old version.
			Check: "[ obj/project.assets.json -nt " + scDotnetProject + " ]",
			Cmd:   "set -e\ndotnet restore",
			Show:  "dotnet restore", Dir: g.Dir,
			Env: append(append([]string{}, scDotnetEnv...), env...),
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
			Cmd: "set -e\n" + scJavaHome + "keytool -importcert -noprompt -alias dbcanvas -file " + g.CA +
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
