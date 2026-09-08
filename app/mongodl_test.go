package main

import (
	"strings"
	"testing"
)

// The download menu item is only as good as its paths, and those paths are
// written by a different function (mongodConfYAML / mongosConfYAML) at deploy. This
// asserts the two agree, so moving the log in the config generator fails here
// rather than silently producing a bundle with no log in it.
func TestMongoDownloadPathsMatchWhatIsDeployed(t *testing.T) {
	mongod := mongodConfYAML("rs0", "", true, "", "")
	want := "path: " + mongoLogPath("")
	if !strings.Contains(mongod, want) {
		t.Errorf("mongod.conf does not log to %q:\n%s", mongoLogPath(""), mongod)
	}
	router := mongosConfYAML("cfg/h1:27017", "")
	want = "path: " + mongoLogPath("mongos")
	if !strings.Contains(router, want) {
		t.Errorf("mongos.conf does not log to %q:\n%s", mongoLogPath("mongos"), router)
	}
	// A mongos must not be handed the mongod path: it does not write that file, and
	// the bundle would be logless on the one node type whose log people ask for most.
	if mongoLogPath("mongos") == mongoLogPath("") {
		t.Error("a mongos and a mongod resolve to the same log file")
	}

	// FTDC lives inside the dbPath the same config sets, which is why the first
	// candidate is the one this app's own nodes hit.
	if !strings.Contains(mongod, "dbPath: "+mongoDataDir) {
		t.Errorf("mongod.conf does not use %s as its dbPath", mongoDataDir)
	}
	if ftdcDiagDirs[0] != mongoDataDir+"/diagnostic.data" {
		t.Errorf("the first FTDC candidate is %q, want it inside the dbPath (%s)", ftdcDiagDirs[0], mongoDataDir)
	}
	// And a mongos candidate exists at all, since it keeps FTDC beside its log
	// rather than in a dbPath it does not have.
	var hasMongos bool
	for _, d := range ftdcDiagDirs {
		if strings.Contains(d, "mongos.diagnostic.data") {
			hasMongos = true
		}
	}
	if !hasMongos {
		t.Error("no mongos FTDC directory among the candidates")
	}
}

// The bundle's name is both the .tar.gz and the one directory inside it, which is
// what somebody ends up with after unpacking three members' captures into the same
// folder — so it has to carry the node and it has to be a safe filename.
func TestMongoBundleName(t *testing.T) {
	if got := mongoBundleName("psmrs-01"); got != "psmrs-01_log_diagnostic_data" {
		t.Errorf("got %q, want psmrs-01_log_diagnostic_data", got)
	}
	if got := mongoBundleName("psmdb-mongos"); got != "psmdb-mongos_log_diagnostic_data" {
		t.Errorf("got %q, want psmdb-mongos_log_diagnostic_data", got)
	}
	// A name that is not filename-safe goes through sanitizeName, the same way it
	// does when it becomes a hostname.
	if got := mongoBundleName("My Node/01"); strings.ContainsAny(got, " /") {
		t.Errorf("got %q, which is not a safe filename", got)
	}
	// No hostname is still a usable name rather than a leading underscore.
	if got := mongoBundleName(""); got != "mongodb_log_diagnostic_data" {
		t.Errorf("got %q, want mongodb_log_diagnostic_data", got)
	}
}

// The menu offers these on MongoDB nodes and the handlers refuse everything else,
// so the two must agree on what a MongoDB node is.
func TestIsMongoNodeType(t *testing.T) {
	for _, ok := range []string{"psm", "psmrs", "psmdb"} {
		if !isMongoNodeType(ok) {
			t.Errorf("%s should be a MongoDB node", ok)
		}
	}
	for _, no := range []string{"ps", "pg", "mclusteradmin", "bighole", "aio", "valkey", ""} {
		if isMongoNodeType(no) {
			t.Errorf("%s should not be a MongoDB node", no)
		}
	}
}
