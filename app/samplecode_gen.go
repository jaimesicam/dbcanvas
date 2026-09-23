package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/template"
)

// samplecode_gen.go — the machinery every generated sample is rendered through.
//
// Each client in the registry owns a template. That is a deliberate choice over a single
// parameterised emitter: the whole value of these samples is that they read like code a person
// would actually write with that driver, and the drivers do not agree about anything — not about
// how a connection is opened, not about how a statement is parameterised, and least of all about
// how TLS is configured. A generator that abstracted over those differences would produce code
// that demonstrates the abstraction rather than the driver.
//
// What *is* shared is here: the connection facts, the scenario expanded into operations, the
// escaping (a password is put into Python, JSON, XML, a shell line and a URI by five different
// rules, and getting that wrong would be a generated program that does not run), and the header
// that says where the code came from and what its dependencies' licences are.
//
// ------------------------------------------------------------------------ what is NOT copied
//
// Every template in this feature is written against the drivers' documented public APIs. None of
// it is lifted from an upstream project's examples or documentation, because DBCanvas is
// GPL-3.0-only and a quickstart copied out of an Apache-2.0 or a GPLv2 project would put
// third-party code in this repository under terms that have to be reasoned about one file at a
// time. Original code against a published API has no such problem. See docs/SAMPLE_CODE.md.

// scGen is everything a template can see.
type scGen struct {
	SampleID   string
	Client     string // the client's label, for the header
	Scenario   scScenario
	Ops        scOps
	Target     scTarget
	Dir        string
	Database   string
	Table      string
	Collection string
	KeyPrefix  string
	// CA is the stack CA on the node, set only when the TLS mode is verify. ClientCert and
	// ClientKey are set only when the sample was asked for mutual TLS; both are paths inside
	// the project directory, and DBCanvas puts the files there when the project is saved.
	CA         string
	ClientCert string
	ClientKey  string
	StorePass  string
	NodeOS     string
	// Deps is the client's dependency list, so a generated manifest (package.json, pom.xml,
	// go.mod) can be rendered from the same metadata the picker showed and the installer used.
	// One list, three consumers, no chance of a project file that asks for something the
	// environment step never installed.
	Deps []scDep
}

// scNewGen assembles the render context for one sample against one endpoint.
func scNewGen(sampleID string, c scClient, s scScenario, t scTarget, nodeOS, clientCertUser string) scGen {
	g := scGen{
		SampleID: sampleID, Client: c.Label, Scenario: s, Ops: scOpsFor(s.ID), Target: t,
		Dir: scProjectDir(sampleID), Database: t.Database, Table: "customers",
		Collection: "customers",
		// A Valkey key that is going to be read back alongside its index set has to land in
		// the same hash slot, or the multi-key command fails on a cluster. The braces are the
		// hash tag that guarantees it, and they are harmless on a standalone node — so every
		// Valkey sample uses them rather than only the cluster ones.
		KeyPrefix: "{" + t.Database + "}:",
		StorePass: scStorePass, NodeOS: nodeOS, Deps: c.Deps,
	}
	if t.TLS.Mode == scTLSVerify {
		g.CA = t.TLS.CA
		if g.CA == "" {
			g.CA = scCAPath(nodeOS)
		}
	}
	if clientCertUser != "" {
		g.ClientCert = g.Dir + "/client-cert.pem"
		g.ClientKey = g.Dir + "/client-key.pem"
	}
	return g
}

// Verify / Encrypted / MTLS are what the templates branch on. Three questions rather than one
// mode string, because every driver answers them with different knobs and several answer only two
// of the three.
func (g scGen) Verify() bool    { return g.Target.TLS.Mode == scTLSVerify }
func (g scGen) Encrypted() bool { return g.Target.TLS.Mode != scTLSOff }
func (g scGen) MTLS() bool      { return g.ClientCert != "" }

// Addr is host:port; Addrs is every address of a multi-host endpoint.
func (g scGen) Addr() string    { return fmt.Sprintf("%s:%d", g.Target.Host, g.Target.Port) }
func (g scGen) Addrs() []string { return g.Target.Addresses() }

// Truststore / Keystore are the PKCS#12 files scPrepareSteps builds for the Java samples, because
// the JDK reads trust material out of a keystore and never out of a PEM file.
func (g scGen) Truststore() string { return g.Dir + "/truststore.p12" }
func (g scGen) Keystore() string   { return g.Dir + "/keystore.p12" }

// ClientKeyDER is the same client key in the one encoding pgJDBC accepts: PKCS#8, DER. It is the
// exception to the keystore rule above — pgJDBC reads the certificate and the key as files, but
// not a PEM key.
func (g scGen) ClientKeyDER() string { return g.Dir + "/client-key.pk8" }

// ClientPEM is the client certificate and its key concatenated into one file, which is what the
// MongoDB drivers ask for instead of two paths. DBCanvas writes all three when a sample is
// generated with mutual TLS, so whichever shape a driver wants is already there.
func (g scGen) ClientPEM() string { return g.Dir + "/client.pem" }

// PgBinDir is where this node's Percona PostgreSQL client binaries live, which is not on PATH on
// the EL images.
func (g scGen) PgBinDir() string { return scPgBinDir(g.NodeOS, g.Target) }

// Header is the banner every generated file carries, in that file's comment syntax.
//
// Four lines, and each one is there because someone will read this file weeks later without the
// page that produced it: which sample it is, which deployment its values came from, and — the one
// that matters operationally — that there is a live password a few lines below.
//
// There is deliberately no licence text here. It used to be three lines of GPLv3 above
// `cur.execute("SELECT VERSION()")`, which was most of a short sample, buried the credentials
// warning under it, and read as DBCanvas claiming ownership of the minimum expression of "connect
// and do CRUD". The project grants the generated projects outright instead, which is stated once
// in docs/SAMPLE_CODE.md rather than recited in every file it writes.
func (g scGen) Header(prefix string) string {
	lines := []string{
		"Generated by DBCanvas — " + g.SampleID,
		"Target: " + g.Addr() + " (" + g.Target.Product + "), as " + g.Target.User + ", TLS " + g.Target.TLS.Mode,
		"",
		"Lab code: the password below is the one DBCanvas generated for this disposable deployment.",
	}
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.TrimRight(prefix+ln, " "))
	}
	return b.String()
}

// scFuncs are the escaping rules. A password from .env is ordinary today and arbitrary tomorrow;
// every one of these is the difference between a program that runs and a syntax error in a
// generated file nobody expected to have to debug.
var scFuncs = template.FuncMap{
	// q quotes for Python, JavaScript, Go and Java alike — Go's own string syntax is a subset
	// all four accept for the characters that occur in a credential.
	"q": strconv.Quote,
	// cs quotes for C#. Not strconv.Quote: Go writes a control byte as \x01, and C#'s \x takes
	// up to four hex digits, so "\x01a" would silently become one character.
	"cs": scCSharpQuote,
	// sq single-quotes for a POSIX shell, closing and reopening around any embedded quote.
	"sq": func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" },
	// urlq escapes for a URI's userinfo or query.
	"urlq": url.QueryEscape,
	// xml escapes for a pom.xml text node.
	"xml": func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
		return r.Replace(s)
	},
	"join": func(sep string, v []string) string { return strings.Join(v, sep) },
	// xmlComment neutralises the one sequence an XML comment cannot contain. The header it
	// wraps is generated from a node label, which is user-supplied text.
	"xmlComment": func(s string) string { return strings.ReplaceAll(s, "--", "- -") },
	// splitCoord splits a Maven "group:artifact" coordinate into the two elements a POM wants.
	"splitCoord": func(s string) []string {
		p := strings.SplitN(s, ":", 2)
		if len(p) == 1 {
			return []string{p[0], p[0]}
		}
		return p
	},
	// The shared shape of the sample data, as template functions, so the four families cannot
	// drift apart on what a customer is.
	"scCustomersDDLMySQL":           scCustomersDDLMySQL,
	"scCustomersDDLPostgres":        scCustomersDDLPostgres,
	"scCustomersDDLMySQLOneLine":    func(t string) string { return scOneLine(scCustomersDDLMySQL(t)) },
	"scCustomersDDLPostgresOneLine": func(t string) string { return scOneLine(scCustomersDDLPostgres(t)) },
	"scDemoName":                    func() string { return scDemoName },
	"scDemoEmail":                   func() string { return scDemoEmail },
	"scDemoNewEmail":                func() string { return scDemoNewEmail },
	"quoted": func(v []string) string {
		out := make([]string, len(v))
		for i, s := range v {
			out[i] = strconv.Quote(s)
		}
		return strings.Join(out, ", ")
	},
}

// scCSharpQuote renders s as a C# regular string literal.
func scCSharpQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// scRender renders one template. A template that fails to parse or execute produces a file whose
// contents are the error rather than a panic or a silent empty file — and TestSampleCodeRenders
// walks every sample in the registry against a synthetic target so that never reaches a user.
func scRender(name, text string, g scGen) string {
	t, err := template.New(name).Funcs(scFuncs).Parse(text)
	if err != nil {
		return "DBCanvas could not parse the template for " + name + ": " + err.Error()
	}
	var b strings.Builder
	if err := t.Execute(&b, g); err != nil {
		return "DBCanvas could not render " + name + ": " + err.Error()
	}
	return b.String()
}

// scFileOf is the one-liner every template entry point uses.
func scFileOf(name, lang, tmpl string, g scGen) scFile {
	return scFile{Name: name, Lang: lang, Body: scRender(name, tmpl, g)}
}

// scClients is the whole registry, assembled from the four engine families. Order matters only
// within a database (see scClientsFor), so the families are listed in picker order.
var scClients = func() []scClient {
	var out []scClient
	out = append(out, scMySQLClients...)
	out = append(out, scPostgresClients...)
	out = append(out, scMongoClients...)
	out = append(out, scValkeyClients...)
	return out
}()

// ------------------------------------------------------------------ shared bits of generated code

// scCustomerSQL is the table every SQL sample works in, in the dialect of each engine. One shape
// across all of them — id, name, email, created_at — so the same program can be read side by side
// in six languages against two engines and the only differences are the ones that matter.
func scCustomersDDLMySQL(table string) string {
	return "CREATE TABLE IF NOT EXISTS " + table + " (\n" +
		"  id INT AUTO_INCREMENT PRIMARY KEY,\n" +
		"  name VARCHAR(100) NOT NULL,\n" +
		"  email VARCHAR(255) NOT NULL,\n" +
		"  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP\n" +
		")"
}

func scCustomersDDLPostgres(table string) string {
	return "CREATE TABLE IF NOT EXISTS " + table + " (\n" +
		"  id SERIAL PRIMARY KEY,\n" +
		"  name TEXT NOT NULL,\n" +
		"  email TEXT NOT NULL,\n" +
		"  created_at TIMESTAMPTZ NOT NULL DEFAULT now()\n" +
		")"
}

// scOneLine flattens a multi-line statement onto one, for the languages whose string literals do
// not span lines without ceremony (Java, and a shell -e argument).
func scOneLine(s string) string {
	out := strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(out, "( ", "(")
}

// The demo row, and the change the Update scenario makes to it. Constants rather than literals in
// twenty-three templates so the output of every sample is comparable line for line.
const (
	scDemoName     = "Alice"
	scDemoEmail    = "alice@example.com"
	scDemoNewEmail = "alice@dbcanvas.example"
)
