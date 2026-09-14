package main

import (
	"strings"
	"testing"
)

// Ledger Sim is the JDBC simulator: what it varies is the client, not the
// server. These cover the parts of that which are decided on the Go side —
// which driver a node ends up using, which targets it may be pointed at, and
// what reaches the container as environment. The URL composition itself is the
// image's, deliberately: see ledgerSimJDBCURL's comment.
//
// Verified live while this was written, against a Percona Server 8.0.46 and a
// Percona Distribution for PostgreSQL 18.6 on the same host: both MySQL drivers
// authenticated against the same caching_sha2_password server, the pool was
// re-pointed from PostgreSQL/pgJDBC to MySQL/Connector-J without restarting the
// process, and the double-entry balance check stayed at zero throughout.

// A driver that cannot speak the engine is a stale form value. The backend must
// agree with what the image will actually load, rather than passing it through
// and letting the JVM silently pick something else.
func TestLedgerSimDriverFollowsTheEngine(t *testing.T) {
	cases := []struct{ engine, asked, want string }{
		{"mysql", "mysql-connector-j", "mysql-connector-j"},
		{"mysql", "mariadb-connector-j", "mariadb-connector-j"},
		{"mysql", "pgjdbc", "mysql-connector-j"}, // cannot speak MySQL
		{"postgres", "pgjdbc", "pgjdbc"},         // the only one that can
		{"postgres", "mysql-connector-j", "pgjdbc"},
		{"postgres", "", "pgjdbc"},
		{"mysql", "", "mysql-connector-j"},
	}
	for _, c := range cases {
		if got := ledgerSimDriverFor(c.engine, c.asked); got != c.want {
			t.Errorf("ledgerSimDriverFor(%q, %q) = %q, want %q", c.engine, c.asked, got, c.want)
		}
	}
}

// Every driver the node can select must have its licence recorded — dbcanvas is
// GPL-3.0-only, and an unrecorded driver is one nobody checked.
func TestLedgerSimEveryDriverHasALicense(t *testing.T) {
	for engine, drivers := range ledgerSimDrivers {
		if !ledgerSimEngineImplemented(engine) {
			t.Errorf("ledgerSimDrivers has an engine %q the node cannot select", engine)
		}
		for _, d := range drivers {
			if ledgerSimDriverLicenses[d] == "" {
				t.Errorf("driver %q has no recorded licence", d)
			}
		}
	}
	// Connector/J is GPLv2-only; it is combinable with this project solely
	// through Oracle's Universal FOSS Exception. If that is ever dropped from
	// the record, the combination stops being defensible.
	if !strings.Contains(ledgerSimDriverLicenses["mysql-connector-j"], "Universal-FOSS-Exception") {
		t.Error("MySQL Connector/J's licence record must name the Universal FOSS Exception")
	}
}

// MongoDB and Valkey have no JDBC driver here. A link to one must be refused with
// a sentence that says where to go instead, not by a JVM failing to load a class.
func TestLedgerSimRefusesTheEnginesJDBCCannotSpeak(t *testing.T) {
	for _, e := range []string{"mongodb", "valkey"} {
		if ledgerSimEngineImplemented(e) {
			t.Errorf("%s must not be an implemented Ledger Sim engine", e)
		}
	}
	for _, e := range ledgerSimEngines {
		if _, ok := ledgerSimDrivers[e]; !ok {
			t.Errorf("engine %q is offered but has no drivers", e)
		}
	}
}

// The manual-mode environment is the whole contract with the image: the names
// here are read by Main.specFromEnv, and a rename on either side is a node that
// deploys pointing at nothing.
func TestLedgerSimManualEnvCarriesTheConnection(t *testing.T) {
	n := designNode{
		LSEngine: "postgres", LSDriver: "pgjdbc", LSHost: "db.example.net", LSPort: 6432,
		LSUser: "ledger", LSPassword: "s3cret", LSDatabase: "books", LSTLS: "require",
		LSParams: "socketTimeout=30000", LSJdbcURL: "",
	}
	env, sec, label := ledgerSimManualEnv(n)
	want := map[string]string{
		"DB_ENGINE": "postgres", "DB_DRIVER": "pgjdbc", "DB_HOST": "db.example.net",
		"DB_PORT": "6432", "DB_USER": "ledger", "DB_PASSWORD": "s3cret",
		"DB_TLS": "require", "DB_PARAMS": "socketTimeout=30000",
	}
	got := map[string]string{}
	for _, kv := range env {
		if i := strings.Index(kv, "="); i > 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
	if sec.Password != "s3cret" || sec.User != "ledger" {
		t.Errorf("credentials must reach the deployment secrets, got %+v", sec)
	}
	if label != "db.example.net:6432" {
		t.Errorf("label = %q", label)
	}
}

// An empty port must become the engine default rather than 0, which no driver
// would accept.
func TestLedgerSimManualEnvFillsInThePort(t *testing.T) {
	for _, c := range []struct {
		engine string
		want   string
	}{{"mysql", "DB_PORT=3306"}, {"postgres", "DB_PORT=5432"}} {
		env, _, _ := ledgerSimManualEnv(designNode{LSEngine: c.engine, LSHost: "h"})
		found := false
		for _, kv := range env {
			if kv == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: env does not contain %s — got %v", c.engine, c.want, env)
		}
	}
}

// A URL override applies in linked mode too: the line says which database, the
// override says how to reach it — that is how a node linked to a cluster gets
// aimed at one member of it.
func TestLedgerSimLinkedEnvHonoursTheURLOverride(t *testing.T) {
	r := stockSimResolved{
		engine: "mysql", host: "pxc01.example.net", port: 3306,
		secrets: stockSimSecrets{User: "app", Password: "app_password"},
	}
	env, sec := ledgerSimLinkedEnv(designNode{LSJdbcURL: "jdbc:mysql://pxc02:3306/ledgersim"}, r)
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "JDBC_URL=jdbc:mysql://pxc02:3306/ledgersim") {
		t.Errorf("the override must reach the container: %v", env)
	}
	// The resolved endpoint still travels, so clearing the override on the
	// dashboard falls back to the database the line actually points at.
	if !strings.Contains(joined, "DB_HOST=pxc01.example.net") {
		t.Errorf("the linked endpoint must still be passed: %v", env)
	}
	if sec.Password != "app_password" {
		t.Errorf("linked credentials must reach the secrets, got %+v", sec)
	}
}

// A password pasted into a URL override must not survive into the non-secret
// config the node panel renders.
func TestLedgerSimMasksAPastedPassword(t *testing.T) {
	got := maskURLPassword("jdbc:postgresql://h:5432/db?user=app&password=hunter2&ssl=true")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password survived masking: %s", got)
	}
	if !strings.Contains(got, "ssl=true") || !strings.Contains(got, "user=app") {
		t.Errorf("masking ate more than the password: %s", got)
	}
}

// The isolation names are a contract with Workload.isolationLevel in the image;
// anything else must be rejected on the canvas rather than ignored by the JVM.
func TestLedgerSimIsolationLevels(t *testing.T) {
	for _, ok := range []string{"", "TRANSACTION_READ_COMMITTED", "TRANSACTION_SERIALIZABLE"} {
		if !ledgerSimIsolationLevels[ok] {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"READ_COMMITTED", "SERIALIZABLE", "repeatable read"} {
		if ledgerSimIsolationLevels[bad] {
			t.Errorf("%q should be rejected — it is not a java.sql.Connection constant", bad)
		}
	}
}

// A node linked to a MongoDB target is a design error with a clear answer, and
// the message has to carry that answer rather than naming a missing driver.
func TestLedgerSimExplainsAnUnsupportedLink(t *testing.T) {
	doc := designDoc{
		Nodes: []designNode{
			{ID: "sim", Type: "ledgersim", Label: "ledger-01"},
			{ID: "db", Type: "psm", Label: "mongo-01"},
		},
		Edges: []designEdge{{ID: "e1", From: edgeEnd{Node: "sim"}, To: edgeEnd{Node: "db"}}},
	}
	engine, issues := ledgerSimEngineAndIssues(doc, doc.Nodes[0])
	if engine != "" {
		t.Errorf("engine = %q, want empty for an unsupported target", engine)
	}
	var msg string
	for _, i := range issues {
		if i.Level == "error" {
			msg = i.Message
		}
	}
	if !strings.Contains(msg, "Stock Market Sim") {
		t.Errorf("the error should point at the sim that can do this, got %q", msg)
	}
}

// An unlinked node in linked mode has nothing to deploy against, and should be
// told so before Deploy rather than after.
func TestLedgerSimNeedsALinkOrAManualConnection(t *testing.T) {
	doc := designDoc{Nodes: []designNode{{ID: "sim", Type: "ledgersim", Label: "ledger-01"}}}
	_, issues := ledgerSimEngineAndIssues(doc, doc.Nodes[0])
	if len(issues) == 0 || issues[0].Level != "error" {
		t.Fatalf("an unlinked linked-mode node must be an error, got %+v", issues)
	}
	// In manual mode with a host, the same node is fine — only an informational
	// note that dbcanvas cannot verify an endpoint it does not manage.
	manual := designNode{ID: "sim", Type: "ledgersim", Label: "ledger-01", LSMode: "manual", LSHost: "db.example.net"}
	_, issues = ledgerSimEngineAndIssues(designDoc{Nodes: []designNode{manual}}, manual)
	for _, i := range issues {
		if i.Level == "error" {
			t.Errorf("a manual node with a host should not error: %q", i.Message)
		}
	}
}

// A Ledger Sim node can run with no pool at all: a fresh connection per
// transaction, closed after it. That is the other half of the comparison the
// node exists to make — plenty of real applications connect that way — so it is
// a first-class mode rather than a degraded one.
//
// Measured while this was written, same workload and same Percona Server 8.0.46,
// switched live: pooled did 23,365 transactions in 30s opening ONE connection
// (p50 8.19ms); direct did 6,097 opening 6,091 (p50 65.54ms, of which 26.75ms
// was the connect itself).

// Anything unrecognised must be "pooled". A typo that silently ran the other
// experiment would make every number on the dashboard mean something else.
func TestLedgerSimPoolModeDefaultsToPooled(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "pooled"},
		{"pooled", "pooled"},
		{"direct", "direct"},
		{"DIRECT", "direct"},
		{" direct ", "direct"},
		{"none", "pooled"},
		{"nopool", "pooled"},
	} {
		if got := ledgerSimPoolMode(designNode{LSPoolMode: c.in}); got != c.want {
			t.Errorf("ledgerSimPoolMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// POOL_MODE is the contract with Main.poolFromEnv; the value has to travel, and
// the pool size must still travel with it so switching back on the dashboard
// finds the node's configured size rather than a default.
func TestLedgerSimPoolModeIsValidated(t *testing.T) {
	n := designNode{ID: "sim", Type: "ledgersim", Label: "ledger-01",
		LSMode: "manual", LSHost: "db.example.net", LSPoolMode: "sideways"}
	var msg string
	for _, i := range ledgerSimIssues(designDoc{Nodes: []designNode{n}}, n) {
		if i.Level == "error" && strings.Contains(i.Message, "connection mode") {
			msg = i.Message
		}
	}
	if msg == "" {
		t.Fatal("an unknown connection mode must be an error on the canvas")
	}
	if !strings.Contains(msg, "pooled") || !strings.Contains(msg, "direct") {
		t.Errorf("the error should name both valid modes, got %q", msg)
	}
}

// Direct mode holds one connection per worker with nothing in front of them, so
// a high worker count is a real risk to a server's max_connections rather than
// just a slow run. It must be said before deploy, not discovered after.
func TestLedgerSimWarnsAboutDirectModeConnectionCount(t *testing.T) {
	n := designNode{ID: "sim", Type: "ledgersim", Label: "ledger-01",
		LSMode: "manual", LSHost: "db.example.net", LSPoolMode: "direct", LSThreads: 64}
	var warned bool
	for _, i := range ledgerSimIssues(designDoc{Nodes: []designNode{n}}, n) {
		if i.Level == "warn" && strings.Contains(i.Message, "max_connections") {
			warned = true
		}
	}
	if !warned {
		t.Error("64 workers with no pool should warn about max_connections")
	}
	// The same node pooled is fine: the pool is the ceiling.
	n.LSPoolMode = "pooled"
	for _, i := range ledgerSimIssues(designDoc{Nodes: []designNode{n}}, n) {
		if strings.Contains(i.Message, "max_connections") {
			t.Errorf("a pooled node should not warn about max_connections: %q", i.Message)
		}
	}
}

// The "workers queue on a smaller pool" note is about a pool, so it must not be
// raised for a node that has none — in direct mode there is no pool to be
// smaller than the worker count.
func TestLedgerSimPoolSizeAdviceIsOnlyForPooledNodes(t *testing.T) {
	n := designNode{ID: "sim", Type: "ledgersim", Label: "ledger-01",
		LSMode: "manual", LSHost: "db.example.net", LSPoolMode: "direct", LSThreads: 8, LSPoolMax: 2}
	for _, i := range ledgerSimIssues(designDoc{Nodes: []designNode{n}}, n) {
		if strings.Contains(i.Message, "pooled connections") {
			t.Errorf("direct mode has no pool to queue on: %q", i.Message)
		}
	}
}
