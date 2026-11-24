package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The series decides the keyring: the plugin on 5.7 / 8.0, the component on 8.4 / 9.7 (where the
// plugin no longer exists).
func TestMySQLKeyringFileOptions(t *testing.T) {
	for _, maj := range []string{"5.7", "8.0"} {
		o := mysqlKeyringFileOptions(maj, "")
		if !strings.Contains(o, "early-plugin-load=keyring_file.so") ||
			!strings.Contains(o, "keyring_file_data="+mysqlKeyringFileData) {
			t.Errorf("%s must load the keyring_file plugin early:\n%s", maj, o)
		}
	}
	for _, maj := range []string{"8.4", "9.7"} {
		o := mysqlKeyringFileOptions(maj, "")
		if strings.Contains(o, "early-plugin-load") || strings.Contains(o, "keyring_file_data") {
			t.Errorf("%s has no keyring plugin — the component is declared by the manifest:\n%s", maj, o)
		}
		if !strings.Contains(o, "default_table_encryption=ON") {
			t.Errorf("%s should encrypt new tables by default:\n%s", maj, o)
		}
	}
}

// default_table_encryption arrived in 8.0.16; an older server refuses to start on it.
func TestDefaultTableEncryptionOnlyWhereItExists(t *testing.T) {
	for _, c := range []struct {
		major, version string
		want           bool
	}{
		{"5.7", "", false},
		{"5.7", "5.7.44-48.1", false},
		{"8.0", "8.0.15-6.1", false},
		{"8.0", "8.0.16-7.1", true},
		{"8.0", "", true},
		{"8.4", "", true},
		{"9.7", "", true},
	} {
		got := strings.Contains(mysqlDefaultTableEncryption(c.major, c.version), "default_table_encryption=ON")
		if got != c.want {
			t.Errorf("%s %q: default_table_encryption=%v, want %v", c.major, c.version, got, c.want)
		}
	}
	if strings.Contains(mysqlVaultOptions("5.7"), "default_table_encryption") {
		t.Error("the 5.7 vault keyring must not set default_table_encryption, which 5.7 does not have")
	}
}

func TestMySQLKeyringFileComponentConf(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(mysqlKeyringFileComponentConf()), &m); err != nil {
		t.Fatalf("component_keyring_file.cnf is not JSON: %v", err)
	}
	if m["path"] != mysqlKeyringComponentData || m["read_only"] != false {
		t.Errorf("unexpected component config: %v", m)
	}
}

// The component's .cnf must go to plugin_dir and the manifest beside mysqld, with the debug and
// XtraBackup copies of the .so excluded — the same rules as the vault component.
func TestMySQLKeyringFileStageScripts(t *testing.T) {
	c := mysqlKeyringFileStageComponentScript
	for _, want := range []string{
		`"$PLUGIN_DIR/component_keyring_file.cnf"`,
		`"$BINDIR/mysqld.my"`,
		`file://component_keyring_file`,
		`! -path '*/debug/*'`, `! -path '*xtrabackup*'`,
		"install -d -o mysql -g mysql -m 0750 " + mysqlKeyringDir,
		// mysqld --initialize runs as root; a keyring file it created would be root-owned.
		"install -o mysql -g mysql -m 0640 /dev/null " + mysqlKeyringComponentData,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("component stage script is missing %q", want)
		}
	}
	if !strings.Contains(mysqlKeyringFileStagePluginScript, "keyring_file.so") ||
		!strings.Contains(mysqlKeyringFileStagePluginScript, "install -d -o mysql -g mysql -m 0750 "+mysqlKeyringDir) {
		t.Error("plugin stage script must check for keyring_file.so and create the keyring directory")
	}
	for _, s := range []string{mysqlKeyringFileStagePluginScript, mysqlKeyringFileStageComponentScript} {
		if strings.Contains(s, "systemctl") || strings.Contains(s, "mysql -u") || strings.Contains(s, "mysql -e") {
			t.Error("staging runs before the server exists; it must not start or query it")
		}
	}
}

func TestMySQLKeyringFileVerifyScript(t *testing.T) {
	s := mysqlKeyringFileVerifyScript
	for _, want := range []string{"keyring_component_status", "PLUGIN_NAME='keyring_file'", "ENCRYPTION='Y'", `[ -s "$KEYRING" ]`} {
		if !strings.Contains(s, want) {
			t.Errorf("verify script is missing %q", want)
		}
	}
}

func TestMySQLKeyringKind(t *testing.T) {
	if k := mysqlKeyringKind(designFrame{}); k != "" {
		t.Errorf("no keyring: got %q", k)
	}
	if k := mysqlKeyringKind(designFrame{KeyringFile: true}); k != "file" {
		t.Errorf("keyring file: got %q", k)
	}
	if k := mysqlKeyringKind(designFrame{EnableVault: true}); k != "vault" {
		t.Errorf("vault: got %q", k)
	}
}

// A PXC cluster with a keyring file needs encrypted cluster traffic exactly as a vault-keyed one
// does: PXC's SST refuses an unencrypted channel once any keyring is configured.
func TestKeyringFilePXCTurnsOnClusterTraffic(t *testing.T) {
	for _, maj := range []string{"8.0", "8.4", "9.7"} {
		frame := designFrame{Type: "pxc", PXCMajor: maj, Label: "c1", KeyringFile: true}
		cnf := pxcMyCnf(frame, designNode{}, "pxc01", "example.net", "pxc01.example.net", mysqlKeyringFileOptions(maj, ""))
		for _, want := range []string{"pxc_encrypt_cluster_traffic=ON", "[sst]", "encrypt=4", "wsrep_sst_method=xtrabackup-v2"} {
			if !strings.Contains(cnf, want) {
				t.Errorf("%s: config is missing %q:\n%s", maj, want, cnf)
			}
		}
		sst := strings.Index(cnf, "[sst]")
		for _, opt := range []string{"default_table_encryption", "early-plugin-load"} {
			if k := strings.Index(cnf, opt); k > sst {
				t.Errorf("%s: %s was written after [sst], where the server never reads it", maj, opt)
			}
		}
	}
}

func TestKeyringFileIssues(t *testing.T) {
	if got := keyringFileIssues("x", "ps", false, true); len(got) != 0 {
		t.Errorf("no keyring file, no issue: %v", got)
	}
	for _, typ := range []string{"ps", "mysqlce", "mysql", "pxc", "mysqlcerepl"} {
		if got := keyringFileIssues("x", typ, true, false); len(got) != 0 {
			t.Errorf("%s with a keyring file should validate: %v", typ, got)
		}
	}
	if got := keyringFileIssues("x", "ps", true, true); len(got) == 0 {
		t.Error("a keyring file and OpenBao together were accepted")
	}
	if got := keyringFileIssues("x", "psm", true, false); len(got) == 0 {
		t.Error("a keyring file on PSMDB was accepted")
	}
	// And XtraBackup is refused with a keyring file as it is with OpenBao.
	if got := xbIssues("x", true, "", "", true, designDoc{}); len(got) == 0 {
		t.Error("XtraBackup with a keyring was accepted")
	}
}
