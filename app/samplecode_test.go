package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// samplecode_test.go — the registry has to hold together, and every sample in it has to render.
//
// The point of these is that the registry is data. A contributor adding PHP + PDO writes a struct
// and a template, and the two things that can go wrong — a template that does not parse, and a
// generated file that still has a placeholder where a real value belongs — are exactly what a
// human reviewer is worst at spotting across twenty-three templates and six scenarios.

// scTestTarget is a resolved endpoint with every field a template can reach, per engine.
func scTestTarget(engine string) scTarget {
	t := scTarget{
		ID: "n1", Label: "db-01", Engine: engine, Kind: "ps", Product: "Percona Server for MySQL",
		Major: "8.4", Host: "db-01.example.net", Port: scDefaultPort(engine),
		User: "app", Password: "app_password", Database: scDemoDatabase, AuthDB: "admin",
		Role: "primary", StackID: 1, StackName: "lab", NodeID: "n1",
	}
	switch engine {
	case scMongoDB:
		t.Kind, t.Product = "psmrs", "Percona Server for MongoDB"
		t.Hosts = []string{"psm-01.example.net", "psm-02.example.net", "psm-03.example.net"}
		t.ReplicaSet = "rs0"
	case scValkey:
		t.Kind, t.Product, t.User = "valkeycluster", "Valkey", "default"
		t.Hosts = []string{"valkey-01.example.net", "valkey-02.example.net", "valkey-03.example.net"}
	case scPostgres:
		t.Kind, t.Product, t.User = "pg", "Percona Distribution for PostgreSQL", "postgres"
	}
	return t
}

// scPlaceholders are the strings a generated sample must never contain. They are the whole reason
// this feature exists: a driver's quickstart has them because it cannot know any better, and
// DBCanvas can.
var scPlaceholders = []string{"localhost", "CHANGEME", "your-database-host", "<host>", "127.0.0.1"}

func TestSampleCodeRendersEverySample(t *testing.T) {
	for _, c := range scClients {
		target := scTestTarget(c.Database)
		for _, s := range scScenarios {
			if !c.offers(s.ID) {
				continue
			}
			for _, tls := range []string{scTLSOff, scTLSRequire, scTLSVerify} {
				for _, mtls := range []string{"", "alice"} {
					tt := scApplyTLSChoice(target, tls, "oraclelinux")
					id := scSampleID(c.Database, c.Language, c.ID, s.ID)
					g := scNewGen(id, c, s, tt, "oraclelinux", mtls)
					files := c.Files(g)
					if len(files) == 0 {
						t.Fatalf("%s: no files rendered", id)
					}
					seen := map[string]bool{}
					for _, f := range files {
						name := id + " " + f.Name + " (tls=" + tls + " mtls=" + mtls + ")"
						if seen[f.Name] {
							t.Errorf("%s: two files with the same name", name)
						}
						seen[f.Name] = true
						if strings.Contains(f.Body, "DBCanvas could not") {
							t.Errorf("%s: template failed: %s", name, lastLines(f.Body, 200))
						}
						if strings.TrimSpace(f.Body) == "" {
							t.Errorf("%s: rendered empty", name)
						}
						for _, p := range scPlaceholders {
							if strings.Contains(f.Body, p) {
								t.Errorf("%s: contains the placeholder %q", name, p)
							}
						}
					}
					if strings.TrimSpace(c.Run(g)) == "" {
						t.Errorf("%s: no run command", id)
					}
				}
			}
		}
	}
}

// The generated code must actually carry the deployment's coordinates — the failure this guards
// against is a template that renders beautifully and connects to nothing.
func TestSampleCodeCarriesTheDeployment(t *testing.T) {
	for _, c := range scClients {
		target := scTestTarget(c.Database)
		id := scSampleID(c.Database, c.Language, c.ID, "crud")
		g := scNewGen(id, c, scScenarios[len(scScenarios)-1], target, "ubuntu", "")
		body := ""
		for _, f := range c.Files(g) {
			body += f.Body
		}
		for _, want := range []string{target.Host, target.Password} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: generated project does not mention %q", id, want)
			}
		}
		// The Valkey clients address the endpoint by host and port separately or as a list;
		// either way the port has to be in there.
		if !strings.Contains(body, g.Addr()) && !strings.Contains(body, "6379") &&
			!strings.Contains(body, "3306") && !strings.Contains(body, "5432") && !strings.Contains(body, "27017") {
			t.Errorf("%s: generated project names no port", id)
		}
	}
}

// Every generated file says where it came from, and none of them recites a licence.
//
// Both halves are load-bearing. The provenance line is what someone reading the file weeks later
// has instead of the page that produced it, and the warning about the password is the one thing
// in the banner that matters operationally. The absence is the other half: DBCanvas grants the
// generated projects outright (see docs/SAMPLE_CODE.md), so a copyleft notice creeping back into
// a twelve-line CRUD example would contradict the grant — and would do it in the file rather
// than anywhere anyone would notice.
func TestSampleCodeFilesCarryProvenanceAndNoLicence(t *testing.T) {
	for _, c := range scClients {
		id := scSampleID(c.Database, c.Language, c.ID, "crud")
		target := scTestTarget(c.Database)
		g := scNewGen(id, c, scScenarios[len(scScenarios)-1], target, "oraclelinux", "")
		for _, f := range c.Files(g) {
			switch f.Name {
			case "requirements.txt":
				// A pip manifest has no comment syntax pip is guaranteed to keep out of a
				// package name, so it carries nothing but the dependencies.
				continue
			case "package.json":
				// JSON has no comments either; the description carries the provenance.
				if !strings.Contains(f.Body, id) {
					t.Errorf("%s: package.json does not name the sample it came from", id)
				}
			default:
				if !strings.Contains(f.Body, "Generated by DBCanvas — "+id) {
					t.Errorf("%s %s: no provenance line", id, f.Name)
				}
				if !strings.Contains(f.Body, target.Host) {
					t.Errorf("%s %s: the banner does not name the deployment it was built against", id, f.Name)
				}
				if !strings.Contains(f.Body, "the password below") {
					t.Errorf("%s %s: the banner does not warn that a real credential is in the file", id, f.Name)
				}
			}
			for _, notice := range []string{"GNU General Public License", "GPL-3.0", "GPLv3", "SPDX-License-Identifier"} {
				if strings.Contains(f.Body, notice) {
					t.Errorf("%s %s: carries %q — the generated projects are granted outright, so nothing here should claim a licence",
						id, f.Name, notice)
				}
			}
		}
	}
}

// The banner is short on purpose: on the Connection Test sample it used to be longer than the
// program it introduced.
func TestSampleCodeHeaderIsShort(t *testing.T) {
	c, _ := scFindClient(scMySQL, "python", "mysql-connector")
	g := scNewGen("mysql/python/mysql-connector/connect", c, scScenarios[0], scTestTarget(scMySQL), "oraclelinux", "")
	if n := len(strings.Split(g.Header("# "), "\n")); n > 5 {
		t.Errorf("the generated banner is %d lines; it belongs above a twelve-line program", n)
	}
}

// The registry's own consistency: unique addresses, a runtime that exists, system packages that
// are registered, and dependency metadata complete enough to answer "what is this and whose is it".
func TestSampleCodeRegistryIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range scClients {
		key := c.Database + "/" + c.Language + "/" + c.ID
		if seen[key] {
			t.Errorf("%s is registered twice", key)
		}
		seen[key] = true

		switch c.Runtime {
		case scRuntimePython, scRuntimeNode, scRuntimeGo, scRuntimeJava, scRuntimeShell:
		default:
			t.Errorf("%s: unknown runtime %q", key, c.Runtime)
		}
		if c.Summary == "" || c.Label == "" {
			t.Errorf("%s: needs a label and a summary — the picker shows both", key)
		}
		for _, id := range c.SysPackages {
			if _, ok := scSysPackages[id]; !ok {
				t.Errorf("%s: names system package %q, which is not in scSysPackages", key, id)
			}
		}
		// A shell sample has no ecosystem dependencies and must name its native client;
		// every other runtime has at least one dependency to install.
		if c.Runtime == scRuntimeShell && len(c.SysPackages) == 0 {
			t.Errorf("%s: a native-client sample must name the package that provides it", key)
		}
		if c.Runtime != scRuntimeShell && len(c.Deps) == 0 {
			t.Errorf("%s: no dependencies declared", key)
		}
		for _, d := range c.Deps {
			if d.License == "" || d.URL == "" {
				t.Errorf("%s: dependency %q must record a licence and an upstream URL", key, d.Name)
			}
			switch d.Manager {
			case "pip":
				if d.Import == "" {
					t.Errorf("%s: pip dependency %q needs an import name for the installed check", key, d.Name)
				}
			case "npm":
			case "gomod", "maven":
				if d.Version == "" {
					t.Errorf("%s: %s dependency %q must be pinned — the manifest needs a version", key, d.Manager, d.Name)
				}
			default:
				t.Errorf("%s: dependency %q has unknown manager %q", key, d.Name, d.Manager)
			}
		}
	}
	// Every database in the picker offers something, and every language that claims a database
	// resolves back through scResolveSample.
	for _, d := range scDatabases {
		clients := scClientsFor(d.ID)
		if len(clients) == 0 {
			t.Errorf("database %q has no clients", d.ID)
		}
		for _, c := range clients {
			for _, s := range scScenarios {
				if !c.offers(s.ID) {
					continue
				}
				id := scSampleID(d.ID, c.Language, c.ID, s.ID)
				if _, _, err := scResolveSample(id); err != nil {
					t.Errorf("%s does not resolve: %v", id, err)
				}
			}
		}
	}
}

// Every database offers at least a shell client and one library client, because "compare the
// driver with the native tool" is the comparison this feature exists to make possible.
func TestSampleCodeEveryDatabaseHasAShellAndALibrary(t *testing.T) {
	for _, d := range scDatabases {
		var shell, lib int
		for _, c := range scClientsFor(d.ID) {
			if c.Runtime == scRuntimeShell {
				shell++
			} else {
				lib++
			}
		}
		if shell == 0 {
			t.Errorf("%s: no native client sample", d.ID)
		}
		if lib < 2 {
			t.Errorf("%s: only %d library samples — the point is comparing them", d.ID, lib)
		}
	}
}

// JDBC and HikariCP are options among many, not the architecture — but both have to be there for
// the two SQL families, and Hikari has to be layered on the same driver rather than a second one.
func TestSampleCodeJDBCAndHikari(t *testing.T) {
	for _, db := range []string{scMySQL, scPostgres} {
		jdbc, okJ := scFindClient(db, "java", "jdbc")
		hik, okH := scFindClient(db, "java", "hikari")
		if !okJ || !okH {
			t.Fatalf("%s: expected both a jdbc and a hikari client", db)
		}
		driver := jdbc.Deps[0].Name
		var found, pool bool
		for _, d := range hik.Deps {
			if d.Name == driver {
				found = true
			}
			if d.Name == "com.zaxxer:HikariCP" {
				pool = true
			}
		}
		if !found {
			t.Errorf("%s: the HikariCP sample does not use the same driver (%s) as the plain JDBC one", db, driver)
		}
		if !pool {
			t.Errorf("%s: the HikariCP sample does not depend on HikariCP", db)
		}
		g := scNewGen(scSampleID(db, "java", "hikari", "crud"), hik, scScenarios[5], scTestTarget(db), "oraclelinux", "")
		body := hik.Files(g)[0].Body
		for _, want := range []string{"HikariConfig", "HikariDataSource", "setMaximumPoolSize", "setMinimumIdle"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s hikari: generated code does not use %s", db, want)
			}
		}
		if !strings.Contains(body, "not a tuning recommendation") {
			t.Errorf("%s hikari: the lab-defaults caveat is missing from the generated code", db)
		}
	}
}

// ------------------------------------------------------------------- the dependency resolver

// The one rule that keeps a sample portable: a sample names packages, and only the resolver knows
// what a package manager is.
func TestSampleCodeInstallsPerDistribution(t *testing.T) {
	cases := []struct{ os, want, notWant string }{
		{"oraclelinux", "dnf -y install", "apt-get"},
		{"ubuntu", "apt-get install -y", "dnf"},
		{"debian", "apt-get install -y", "dnf"},
	}
	for _, c := range scClients {
		g := scNewGen(scSampleID(c.Database, c.Language, c.ID, "crud"), c, scScenarios[5], scTestTarget(c.Database), "oraclelinux", "")
		for _, tc := range cases {
			plan := scBuildPlan(c, g, tc.os, false)
			if len(plan.System) == 0 {
				t.Errorf("%s/%s on %s: nothing to install, not even a runtime", c.Database, c.ID, tc.os)
			}
			for _, s := range plan.System {
				if !strings.Contains(s.Show, tc.want) {
					t.Errorf("%s/%s on %s: step %q does not use %q", c.Database, c.ID, tc.os, s.Label, tc.want)
				}
				if strings.Contains(s.Show, tc.notWant) {
					t.Errorf("%s/%s on %s: step %q reaches for %q", c.Database, c.ID, tc.os, s.Label, tc.notWant)
				}
				if s.Check == "" {
					t.Errorf("%s/%s on %s: step %q has no check, so it would reinstall every run",
						c.Database, c.ID, tc.os, s.Label)
				}
			}
		}
	}
}

// Nothing is reinstalled on a second run: every step that can be skipped carries a check, and the
// run step is the only one without one.
func TestSampleCodeEveryStepIsCheckedExceptTheRun(t *testing.T) {
	for _, c := range scClients {
		g := scNewGen(scSampleID(c.Database, c.Language, c.ID, "crud"), c, scScenarios[5], scTestTarget(c.Database), "ubuntu", "alice")
		plan := scBuildPlan(c, g, "ubuntu", false)
		for _, s := range append(append(append([]scStep{}, plan.System...), plan.Deps...), plan.Prepare...) {
			// The Go module resolve is the documented exception: DBCanvas rewrites go.mod
			// on every save, so no check can survive it — and `go mod tidy` against a warm
			// module cache downloads nothing. See scDepSteps.
			if s.ID == "gomod" {
				continue
			}
			if s.Check == "" {
				t.Errorf("%s/%s: step %q would run again on every invocation", c.Database, c.ID, s.Label)
			}
		}
		if plan.Run.Cmd == "" {
			t.Errorf("%s/%s: no run command in the plan", c.Database, c.ID)
		}
		if plan.Dir == "" || !strings.HasPrefix(plan.Dir, scRoot+"/") {
			t.Errorf("%s/%s: project directory %q is not under %s", c.Database, c.ID, plan.Dir, scRoot)
		}
	}
}

// The proxy the node was deployed with has to reach the ecosystem installers too: dnf and apt were
// configured at deploy, pip and npm were not.
func TestSampleCodePlanCarriesTheProxy(t *testing.T) {
	c, _ := scFindClient(scMySQL, "python", "mysql-connector")
	g := scNewGen("mysql/python/mysql-connector/crud", c, scScenarios[5], scTestTarget(scMySQL), "oraclelinux", "")
	plan := scBuildPlan(c, g, "oraclelinux", true)
	var sawSystem, sawDep bool
	for _, s := range plan.System {
		for _, e := range s.Env {
			if strings.HasPrefix(e, "PROXY=http://intranet.") {
				sawSystem = true
			}
		}
	}
	for _, s := range plan.Deps {
		for _, e := range s.Env {
			if strings.HasPrefix(e, "https_proxy=http://intranet.") {
				sawDep = true
			}
		}
	}
	if !sawSystem {
		t.Error("a proxied node's package install does not carry PROXY")
	}
	if !sawDep {
		t.Error("a proxied node's pip install does not carry https_proxy")
	}
}

// Reset must never be able to remove anything outside the samples root, whatever it is handed.
func TestSampleCodeResetIsConfinedToTheSamplesRoot(t *testing.T) {
	script := scResetScript("/etc")
	if !strings.Contains(script, "refusing to remove a directory outside") {
		t.Fatal("the reset script has no guard")
	}
	if !strings.Contains(scResetScript(scRoot+"/mysql-python"), scRoot+"/mysql-python") {
		t.Error("the reset script does not name the directory it removes")
	}
}

// ------------------------------------------------------------------------------- TLS

func TestSampleCodeTLSDerivation(t *testing.T) {
	cases := []struct {
		engine string
		cert   bool
		want   string
	}{
		{scMySQL, true, scTLSVerify},
		// MySQL generates its own certificate at initialization, so the connection can be
		// encrypted even when DBCanvas signed nothing — it just cannot be verified.
		{scMySQL, false, scTLSRequire},
		{scPostgres, true, scTLSVerify},
		{scPostgres, false, scTLSOff},
		// A MongoDB node carries the certificate but does not serve TLS until an operator
		// turns it on across the whole set, so the honest default is off.
		{scMongoDB, true, scTLSOff},
		{scMongoDB, false, scTLSOff},
		{scValkey, false, scTLSOff},
	}
	for _, tc := range cases {
		got := scDeriveTLS(tc.engine, tc.cert, "oraclelinux")
		if got.Mode != tc.want {
			t.Errorf("%s generateCert=%v: got %q, want %q", tc.engine, tc.cert, got.Mode, tc.want)
		}
		if got.Why == "" {
			t.Errorf("%s generateCert=%v: no reason given", tc.engine, tc.cert)
		}
		if got.Mode == scTLSVerify && got.CA == "" {
			t.Errorf("%s: verify mode with no CA path", tc.engine)
		}
	}
	if ca := scCAPath("ubuntu"); ca != "/usr/local/share/ca-certificates/dbcanvas-ca.crt" {
		t.Errorf("Debian CA path is %q", ca)
	}
	if ca := scCAPath("oraclelinux"); ca != "/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt" {
		t.Errorf("EL CA path is %q", ca)
	}
}

// A TLS choice on the page overrides what was derived, and verify always lands with a CA path —
// the generated code is otherwise verifying against nothing.
func TestSampleCodeTLSOverride(t *testing.T) {
	base := scDeriveTLS(scPostgres, false, "ubuntu")
	if base.Mode != scTLSOff {
		t.Fatalf("expected off, got %q", base.Mode)
	}
	tgt := scApplyTLSChoice(scTarget{TLS: base}, scTLSVerify, "ubuntu")
	if tgt.TLS.Mode != scTLSVerify || tgt.TLS.CA == "" {
		t.Errorf("override to verify gave %+v", tgt.TLS)
	}
	if got := scApplyTLSChoice(tgt, "nonsense", "ubuntu"); got.TLS.Mode != scTLSVerify {
		t.Errorf("an unrecognised mode changed the posture to %q", got.TLS.Mode)
	}
}

// Each driver spells TLS its own way, and generating one spelling for all of them is the mistake
// this feature is specifically meant not to make.
func TestSampleCodeTLSSyntaxIsPerDriver(t *testing.T) {
	cases := []struct {
		db, lang, client string
		want             []string
	}{
		{scMySQL, "python", "mysql-connector", []string{"ssl_verify_identity", "ssl_ca"}},
		{scMySQL, "python", "pymysql", []string{"check_hostname"}},
		{scMySQL, "node", "mysql2", []string{"rejectUnauthorized: true"}},
		{scMySQL, "go", "database-sql", []string{"RegisterTLSConfig", "RootCAs"}},
		{scMySQL, "java", "jdbc", []string{"sslMode=VERIFY_IDENTITY", "trustCertificateKeyStoreUrl"}},
		{scMySQL, "shell", "mysql", []string{"--ssl-mode=VERIFY_IDENTITY", "--ssl-ca"}},
		{scPostgres, "python", "psycopg", []string{"verify-full", "sslrootcert"}},
		{scPostgres, "java", "jdbc", []string{"sslmode=verify-full", "sslrootcert="}},
		{scPostgres, "shell", "psql", []string{"PGSSLMODE=verify-full", "PGSSLROOTCERT"}},
		{scMongoDB, "python", "pymongo", []string{"tlsCAFile"}},
		{scMongoDB, "java", "mongodb-driver", []string{"javax.net.ssl.trustStore"}},
		{scMongoDB, "shell", "mongosh", []string{"--tlsCAFile"}},
	}
	for _, tc := range cases {
		c, ok := scFindClient(tc.db, tc.lang, tc.client)
		if !ok {
			t.Fatalf("%s/%s/%s is not registered", tc.db, tc.lang, tc.client)
		}
		target := scApplyTLSChoice(scTestTarget(tc.db), scTLSVerify, "oraclelinux")
		g := scNewGen(scSampleID(tc.db, tc.lang, tc.client, "crud"), c, scScenarios[5], target, "oraclelinux", "")
		body := ""
		for _, f := range c.Files(g) {
			body += f.Body
		}
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s/%s/%s: verified TLS does not produce %q", tc.db, tc.lang, tc.client, want)
			}
		}
		// The CA reaches the client either directly, as a path in the generated code, or —
		// on the JVM, which will not read a PEM — through the truststore the prepare step
		// builds from exactly that path. Both count; neither being true does not.
		plan := scBuildPlan(c, g, "oraclelinux", false)
		viaPrepare := false
		for _, s := range plan.Prepare {
			if strings.Contains(s.Cmd, scCAPath("oraclelinux")) {
				viaPrepare = true
			}
		}
		if !strings.Contains(body, scCAPath("oraclelinux")) && !viaPrepare {
			t.Errorf("%s/%s/%s: the CA on this node reaches the client by no route at all", tc.db, tc.lang, tc.client)
		}
	}
}

// A Java sample that verifies TLS needs a keystore, because the JVM will not read a PEM — except
// pgJDBC, which reads the PEM itself and must not be given a pointless keytool step.
func TestSampleCodeJavaTrustMaterial(t *testing.T) {
	mysql, _ := scFindClient(scMySQL, "java", "jdbc")
	g := scNewGen("mysql/java/jdbc/crud", mysql, scScenarios[5],
		scApplyTLSChoice(scTestTarget(scMySQL), scTLSVerify, "oraclelinux"), "oraclelinux", "")
	plan := scBuildPlan(mysql, g, "oraclelinux", false)
	if len(plan.Prepare) != 1 || !strings.Contains(plan.Prepare[0].Show, "keytool") {
		t.Errorf("Connector/J with verify should build a truststore, got %+v", plan.Prepare)
	}

	pg, _ := scFindClient(scPostgres, "java", "jdbc")
	gp := scNewGen("postgres/java/jdbc/crud", pg, scScenarios[5],
		scApplyTLSChoice(scTestTarget(scPostgres), scTLSVerify, "oraclelinux"), "oraclelinux", "")
	if steps := scBuildPlan(pg, gp, "oraclelinux", false).Prepare; len(steps) != 0 {
		t.Errorf("pgJDBC reads a PEM CA directly and needs no keystore, got %+v", steps)
	}

	// Mutual TLS is the case where pgJDBC does need a conversion: it will not read a PEM key.
	gpm := scNewGen("postgres/java/jdbc/crud", pg, scScenarios[5],
		scApplyTLSChoice(scTestTarget(scPostgres), scTLSVerify, "oraclelinux"), "oraclelinux", "alice")
	steps := scBuildPlan(pg, gpm, "oraclelinux", false).Prepare
	if len(steps) != 1 || !strings.Contains(steps[0].Show, "pkcs8") {
		t.Errorf("pgJDBC with a client certificate should convert the key to PKCS#8 DER, got %+v", steps)
	}
}

// ------------------------------------------------------------------------------- addressing

func TestSampleCodeSampleIDRoundTrip(t *testing.T) {
	id := scSampleID("mysql", "java", "hikari", "crud")
	if id != "mysql/java/hikari/crud" {
		t.Fatalf("id is %q", id)
	}
	db, lang, client, scenario, ok := scParseSampleID(id)
	if !ok || db != "mysql" || lang != "java" || client != "hikari" || scenario != "crud" {
		t.Fatalf("round trip gave %q %q %q %q ok=%v", db, lang, client, scenario, ok)
	}
	for _, bad := range []string{"", "mysql", "mysql/java/hikari", "mysql//hikari/crud", "a/b/c/d/e"} {
		if _, _, _, _, ok := scParseSampleID(bad); ok {
			t.Errorf("%q parsed as a sample id", bad)
		}
	}
	// The three ways to be wrong get three different sentences.
	for _, tc := range []struct{ id, want string }{
		{"nosuchdb/python/x/crud", "no samples for database"},
		{"mysql/python/nosuchclient/crud", "no python sample for mysql"},
		{"mysql/python/pymysql/nosuchscenario", "unknown example"},
	} {
		_, _, err := scResolveSample(tc.id)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want something about %q", tc.id, err, tc.want)
		}
	}
}

// Two samples must never share a project directory, or one would overwrite the other's manifest.
func TestSampleCodeProjectDirsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, c := range scClients {
		for _, s := range scScenarios {
			if !c.offers(s.ID) {
				continue
			}
			id := scSampleID(c.Database, c.Language, c.ID, s.ID)
			dir := scProjectDir(id)
			if other, dup := seen[dir]; dup {
				t.Errorf("%s and %s share the directory %s", id, other, dir)
			}
			seen[dir] = id
		}
	}
}

// The scenarios expand to the operations the output is built around, and "Read" must never read a
// row nothing inserted.
func TestSampleCodeScenarioOps(t *testing.T) {
	if ops := scOpsFor("connect"); ops.Any() {
		t.Errorf("connect should do nothing but connect, got %+v", ops)
	}
	for _, s := range []string{"read", "update", "delete"} {
		ops := scOpsFor(s)
		if !ops.Seed {
			t.Errorf("%s: nothing is seeded, so there is nothing to %s", s, s)
		}
		if ops.Create {
			t.Errorf("%s: an insert it does not demonstrate should be reported as a seed", s)
		}
	}
	crud := scOpsFor("crud")
	if !crud.Create || !crud.Read || !crud.Update || !crud.Delete || !crud.Schema {
		t.Errorf("Full CRUD is not complete: %+v", crud)
	}
	if crud.Seed {
		t.Error("Full CRUD creates its own row; it should not also seed one")
	}
}

// The catalogue the picker reads has to carry what a choice implies before it is made.
func TestSampleCodeCatalogue(t *testing.T) {
	cat := scCatalog()
	if len(cat.Databases) != len(scDatabases) {
		t.Fatalf("catalogue has %d databases, registry has %d", len(cat.Databases), len(scDatabases))
	}
	for _, d := range cat.Databases {
		if len(d.Clients) == 0 {
			t.Errorf("%s: no clients in the catalogue", d.ID)
		}
		for _, c := range d.Clients {
			if len(c.Scenarios) == 0 {
				t.Errorf("%s/%s: no scenarios", d.ID, c.ID)
			}
			if len(c.Requires) == 0 {
				t.Errorf("%s/%s: says it needs nothing installed", d.ID, c.ID)
			}
			if c.LanguageLabel == "" {
				t.Errorf("%s/%s: no language label for the picker", d.ID, c.ID)
			}
		}
	}
}

// Irrelevant combinations must be unreachable: a client is registered against exactly one database,
// so the picker filtered by database can never offer a driver that cannot speak it.
func TestSampleCodeClientsAreScopedToOneDatabase(t *testing.T) {
	for _, d := range scDatabases {
		for _, c := range scClientsFor(d.ID) {
			if c.Database != d.ID {
				t.Errorf("%s/%s is listed under %s", c.Database, c.ID, d.ID)
			}
		}
	}
	// The combination the brief calls out by name: MySQL offers no psycopg, PostgreSQL no mysql2.
	if _, ok := scFindClient(scMySQL, "python", "psycopg"); ok {
		t.Error("psycopg is offered for MySQL")
	}
	if _, ok := scFindClient(scPostgres, "node", "mysql2"); ok {
		t.Error("mysql2 is offered for PostgreSQL")
	}
	if _, ok := scFindClient(scMongoDB, "java", "jdbc"); ok {
		t.Error("JDBC is offered for MongoDB")
	}
}

// The page reads these by name, and a Go struct with no tag serialises its field names
// capitalised — which is not a compile error, not a test failure anywhere else, and an empty
// dropdown in the browser. Found exactly that way, on the first live request.
func TestSampleCodeCatalogueSerialisesForThePage(t *testing.T) {
	b, err := json.Marshal(scCatalog())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	scenarios, _ := out["scenarios"].([]any)
	if len(scenarios) == 0 {
		t.Fatal("no scenarios in the catalogue JSON")
	}
	for _, want := range []string{"id", "label", "blurb"} {
		if _, ok := scenarios[0].(map[string]any)[want]; !ok {
			t.Errorf("a scenario has no %q key — the Example picker reads it", want)
		}
	}
	dbs, _ := out["databases"].([]any)
	if len(dbs) == 0 {
		t.Fatal("no databases in the catalogue JSON")
	}
	db, _ := dbs[0].(map[string]any)
	for _, want := range []string{"id", "label", "clients"} {
		if _, ok := db[want]; !ok {
			t.Errorf("a database has no %q key", want)
		}
	}
	clients, _ := db["clients"].([]any)
	client, _ := clients[0].(map[string]any)
	for _, want := range []string{"id", "language", "languageLabel", "label", "summary", "scenarios", "requires", "deps"} {
		if _, ok := client[want]; !ok {
			t.Errorf("a client has no %q key — the picker reads it", want)
		}
	}
	// And the same for an endpoint, which the Target picker reads.
	tb, _ := json.Marshal(scTestTarget(scMySQL).dto())
	var tgt map[string]any
	json.Unmarshal(tb, &tgt)
	for _, want := range []string{"id", "label", "engine", "host", "port", "user", "database", "tls"} {
		if _, ok := tgt[want]; !ok {
			t.Errorf("an endpoint has no %q key", want)
		}
	}
	if tls, _ := tgt["tls"].(map[string]any); tls["mode"] == nil || tls["why"] == nil {
		t.Errorf("the TLS posture does not serialise its mode and its reason: %v", tgt["tls"])
	}
}
