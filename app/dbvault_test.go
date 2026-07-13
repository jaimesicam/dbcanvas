package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The keyring component only exists from Percona Server 8.4, and only PS 5.7 is stuck on KV v1
// (its plugin predates the v2 API). Each database also gets its own mount — Percona is explicit
// that a secret_mount_point must be used by a single server.
func TestVaultMountFor(t *testing.T) {
	tests := []struct {
		name          string
		node          designNode
		wantMount     string
		wantKV        string
		wantKVVersion string
	}{
		{"PS 5.7 → KV v1", designNode{Type: "ps", PSMajor: "5.7"}, "mysql-ps01", "kv", "1"},
		{"PS 8.0 → KV v2", designNode{Type: "ps", PSMajor: "8.0"}, "mysql-ps01", "kv-v2", "2"},
		{"PS 8.4 → KV v2", designNode{Type: "ps", PSMajor: "8.4"}, "mysql-ps01", "kv-v2", "2"},
		{"PSMDB → KV v2", designNode{Type: "psm"}, "mongodb-ps01", "kv-v2", "2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mount, kv, version := vaultMountFor(tc.node, "ps01")
			if mount != tc.wantMount || kv != tc.wantKV || version != tc.wantKVVersion {
				t.Errorf("got (%s, %s, v%s), want (%s, %s, v%s)", mount, kv, version, tc.wantMount, tc.wantKV, tc.wantKVVersion)
			}
		})
	}
}

// The 5.7 plugin has no secret_mount_point_version option at all; 8.0 needs it to reach a KV v2
// mount. vault_ca points at the CA already in the node's trust store — nothing is copied.
func TestMySQLKeyringPluginConf(t *testing.T) {
	v2 := mysqlKeyringPluginConf("https://bao01.example.net:8200", "mysql-ps01", "tok", intranetCAAnchor, "2")
	for _, want := range []string{
		"vault_url = https://bao01.example.net:8200",
		"secret_mount_point = mysql-ps01",
		"token = tok",
		"vault_ca = " + intranetCAAnchor,
		"secret_mount_point_version = 2",
	} {
		if !strings.Contains(v2, want) {
			t.Errorf("8.0 plugin conf missing %q\n%s", want, v2)
		}
	}
	v1 := mysqlKeyringPluginConf("https://bao01.example.net:8200", "mysql-ps01", "tok", intranetCAAnchor, "1")
	if strings.Contains(v1, "secret_mount_point_version") {
		t.Errorf("Percona Server 5.7 has no secret_mount_point_version option:\n%s", v1)
	}
	// A plain-HTTP OpenBao has no CA to verify.
	if noCA := mysqlKeyringPluginConf("http://bao01.example.net:8200", "mysql-ps01", "tok", "", "2"); strings.Contains(noCA, "vault_ca") {
		t.Errorf("no CA should be named for a non-TLS OpenBao:\n%s", noCA)
	}
}

// The 8.4 component config is JSON and autodetects the KV version.
func TestMySQLKeyringComponentConf(t *testing.T) {
	out := mysqlKeyringComponentConf("https://bao01.example.net:8200", "mysql-ps01", "tok", intranetCAAnchor)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("component conf is not valid JSON: %v\n%s", err, out)
	}
	if m["vault_url"] != "https://bao01.example.net:8200" || m["secret_mount_point"] != "mysql-ps01" ||
		m["token"] != "tok" || m["secret_mount_point_version"] != "AUTO" || m["vault_ca"] != intranetCAAnchor {
		t.Errorf("unexpected component conf: %s", out)
	}
	if noCA := mysqlKeyringComponentConf("http://bao01.example.net:8200", "mysql-ps01", "tok", ""); strings.Contains(noCA, "vault_ca") {
		t.Errorf("no CA should be named for a non-TLS OpenBao:\n%s", noCA)
	}
}

// Both of these were caught against real servers and are easy to reintroduce:
//
//   - Percona Server's packaged /etc/my.cnf has no `!includedir`, so a my.cnf.d drop-in is never
//     read and the plugin silently never loads. The options must go into /etc/my.cnf.
//   - The 8.4 component reads its config from plugin_dir, NOT from beside the mysqld binary
//     (only the manifest goes there). Get it wrong and the component loads *Disabled*, then the
//     first encrypted table kills the server. Verification must therefore check Component_status
//     is Active, not merely that the component is named in the status table.
func TestMySQLKeyringScriptsUseTheRightFiles(t *testing.T) {
	if strings.Contains(mysqlKeyringPluginScript, "my.cnf.d") {
		t.Error("the plugin options must go in /etc/my.cnf — Percona Server's my.cnf has no !includedir, so a my.cnf.d drop-in is never read")
	}
	if !strings.Contains(mysqlKeyringPluginScript, "CNF=/etc/my.cnf") ||
		!strings.Contains(mysqlKeyringPluginScript, "early-plugin-load=keyring_vault.so") {
		t.Errorf("the plugin script must append early-plugin-load to /etc/my.cnf:\n%s", mysqlKeyringPluginScript)
	}

	if !strings.Contains(mysqlKeyringComponentScript, `PLUGIN_DIR=$(mysql -N -e "SELECT @@plugin_dir"`) ||
		!strings.Contains(mysqlKeyringComponentScript, `"$PLUGIN_DIR/component_keyring_vault.cnf"`) {
		t.Errorf("the component config must be written into plugin_dir:\n%s", mysqlKeyringComponentScript)
	}
	if !strings.Contains(mysqlKeyringComponentScript, `"$BINDIR/mysqld.my"`) {
		t.Errorf("the global manifest must sit beside the mysqld binary:\n%s", mysqlKeyringComponentScript)
	}
	if !strings.Contains(mysqlKeyringComponentScript, `[ "$STATUS" = "Active" ]`) {
		t.Errorf("the component must be verified as Active — a Disabled component crashes the server on the first encrypted table:\n%s", mysqlKeyringComponentScript)
	}
}

// mongod's vault block must sit inside `security:` (two-space keys), name the KV v2 data path,
// and verify TLS with the Intranet CA already on the node.
func TestMongoVaultBlockInConf(t *testing.T) {
	block := mongoVaultBlock("bao01.example.net", "mongodb-psm01/data/psm01", intranetCAAnchor, true)
	conf := mongodConfYAML("", "", false, "", block)
	for _, want := range []string{
		"security:\n  authorization: enabled\n  enableEncryption: true\n  vault:\n",
		"    serverName: bao01.example.net",
		"    port: 8200",
		"    secret: mongodb-psm01/data/psm01",
		"    tokenFile: " + mongoVaultTokenFile,
		"    serverCAFile: " + intranetCAAnchor,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("mongod.conf missing %q\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "disableTLSForTesting") {
		t.Errorf("a TLS OpenBao must not disable TLS:\n%s", conf)
	}

	plain := mongoVaultBlock("bao01.example.net", "mongodb-psm01/data/psm01", "", false)
	if !strings.Contains(plain, "disableTLSForTesting: true") || strings.Contains(plain, "serverCAFile") {
		t.Errorf("a non-TLS OpenBao needs disableTLSForTesting and no CA:\n%s", plain)
	}

	// A node with no encryption gets no vault block at all.
	if off := mongodConfYAML("rs0", "", true, "", ""); strings.Contains(off, "enableEncryption") {
		t.Errorf("mongod.conf should carry no encryption when vault is off:\n%s", off)
	}
}

func TestVaultIssues(t *testing.T) {
	bao := map[string]bool{"bao1": true}
	if got := vaultIssues(designNode{Type: "ps", Label: "ps01"}, bao); len(got) != 0 {
		t.Errorf("encryption off should raise nothing, got %q", got[0].Message)
	}
	if got := vaultIssues(designNode{Type: "ps", Label: "ps01", EnableVault: true, OpenBaoNodeID: "bao1"}, bao); len(got) != 0 {
		t.Errorf("a linked OpenBao node is valid, got %q", got[0].Message)
	}
	if got := vaultIssues(designNode{Type: "psm", Label: "psm01", EnableVault: true}, bao); len(got) == 0 {
		t.Error("encryption with no OpenBao node selected must be an error")
	}
	if got := vaultIssues(designNode{Type: "psm", Label: "psm01", EnableVault: true, OpenBaoNodeID: "gone"}, bao); len(got) == 0 {
		t.Error("encryption linked to a node that is not on the canvas must be an error")
	}
}
