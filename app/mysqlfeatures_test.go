package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestMySQLHas(t *testing.T) {
	for _, c := range []struct {
		key, major, version string
		want                bool
	}{
		{featDefaultTableEncryption, "5.7", "", false},
		{featDefaultTableEncryption, "8.0", "8.0.15-6.1", false},
		{featDefaultTableEncryption, "8.0", "8.0.16-7.1", true},
		{featDefaultTableEncryption, "8.0", "", true},
		{featDefaultTableEncryption, "9.7", "", true},
		{featBinlogEncryption, "8.0", "8.0.13", false},
		{featBinlogEncryption, "8.0", "8.0.14", true},
		{featClone, "8.0", "8.0.16-7.1", false},
		{featClone, "8.0", "8.0.17", true},
		{featKeyringPlugins, "8.0", "", true},
		{featKeyringPlugins, "8.4", "", false},
		{featKeyringPlugins, "9.7", "", false},
		{featKeyringComponents, "8.0", "", false},
		{featKeyringComponents, "8.4", "8.4.0-1", true},
		{"no-such-feature", "8.4", "", false},
	} {
		if got := mysqlHas(c.key, c.major, c.version); got != c.want {
			t.Errorf("mysqlHas(%s, %s, %q) = %v, want %v", c.key, c.major, c.version, got, c.want)
		}
	}
	if !cloneCapable("8.0.36-28") || cloneCapable("8.0.16-7") || cloneCapable("") {
		t.Error("cloneCapable no longer agrees with the table")
	}
}

// Every key the code asks about is in the table, and the table has no duplicates.
func TestMySQLFeatureTable(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range mysqlFeatures {
		if seen[f.Key] {
			t.Errorf("%s twice", f.Key)
		}
		seen[f.Key] = true
		if f.Label == "" {
			t.Errorf("%s has no label", f.Key)
		}
	}
	for _, k := range []string{featDefaultTableEncryption, featBinlogEncryption, featRedoLogEncrypt, featUndoLogEncrypt,
		featTempTablespaceEncrypt, featClone, featKeyringPlugins, featKeyringComponents} {
		if !seen[k] {
			t.Errorf("%s is asked about but not in the table", k)
		}
	}
}

// The designer imports the table as web/src/lib/mysqlFeatures.json (it ships in the same binary,
// so it cannot be older than the server). This keeps that copy in step with mysqlFeatures:
// DBCANVAS_UPDATE_GOLDEN=1 go test -run TestMySQLFeaturesJSON rewrites it.
func TestMySQLFeaturesJSON(t *testing.T) {
	const path = "web/src/lib/mysqlFeatures.json"
	want, err := json.MarshalIndent(mysqlFeatures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("DBCANVAS_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s is out of date with mysqlFeatures — run DBCANVAS_UPDATE_GOLDEN=1 go test -run TestMySQLFeaturesJSON", path)
	}
}
