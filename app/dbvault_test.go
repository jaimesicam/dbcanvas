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

// The keyring is staged BEFORE mysqld first starts, which is what the scripts have to be
// checked against: nothing may restart the server, and nothing may ask a running server a
// question — on a cluster member neither is available, and a restart to pick up a keyring is a
// member leaving the cluster.
func TestMySQLKeyringStageScriptsRunBeforeTheServerDoes(t *testing.T) {
	for name, script := range map[string]string{
		"plugin":    mysqlKeyringStagePluginScript,
		"component": mysqlKeyringStageComponentScript,
	} {
		if strings.Contains(script, "systemctl restart") || strings.Contains(script, "mysqladmin ping") {
			t.Errorf("the %s staging script starts or restarts the server:\n%s", name, script)
		}
		if strings.Contains(script, "mysql -N -e") {
			t.Errorf("the %s staging script queries a server that is not running yet:\n%s", name, script)
		}
	}

	// The plugin's config carries the Vault token, and the plugin refuses a config any other
	// user can read.
	if !strings.Contains(mysqlKeyringStagePluginScript, "chmod 0600 "+mysqlKeyringConf) {
		t.Errorf("the plugin config must be 0600 — it holds the token:\n%s", mysqlKeyringStagePluginScript)
	}
	// And the options that LOAD the plugin are not in the script at all: they belong in the
	// node's own config file, which is the only one that is right on both OS families.
	if strings.Contains(mysqlKeyringStagePluginScript, "early-plugin-load") {
		t.Error("early-plugin-load belongs in the rendered my.cnf (mysqlVaultOptions), not appended to /etc/my.cnf")
	}

	// The two component files do not live in the same place: the manifest beside the binary,
	// the config in plugin_dir — which is found from the component's own .so, because there is
	// no running server to ask for @@plugin_dir.
	if !strings.Contains(mysqlKeyringStageComponentScript, `"$BINDIR/mysqld.my"`) {
		t.Errorf("the global manifest must sit beside the mysqld binary:\n%s", mysqlKeyringStageComponentScript)
	}
	if !strings.Contains(mysqlKeyringStageComponentScript, `"$PLUGIN_DIR/component_keyring_vault.cnf"`) ||
		!strings.Contains(mysqlKeyringStageComponentScript, `mysqld --no-defaults --verbose --help`) {
		t.Errorf("the component config must go to the plugin_dir the binary itself reports:\n%s", mysqlKeyringStageComponentScript)
	}
	// A PXC node ships three copies of component_keyring_vault.so — the server's, the debug
	// build beside it, and XtraBackup's — and `find -print -quit` returns the DEBUG one first.
	// A live bootstrap died on exactly that: "Keyring configuration doesn't exists:
	// /usr/lib64/mysql/plugin//component_keyring_vault.cnf". The fallback search must exclude
	// both wrong trees.
	for _, excl := range []string{`! -path '*/debug/*'`, `! -path '*xtrabackup*'`} {
		if !strings.Contains(mysqlKeyringStageComponentScript, excl) {
			t.Errorf("the plugin_dir fallback does not exclude %s:\n%s", excl, mysqlKeyringStageComponentScript)
		}
	}
	// And whichever directory was chosen has to actually hold the component, or the server is
	// told to load something that is not there.
	if !strings.Contains(mysqlKeyringStageComponentScript, `[ -f "$PLUGIN_DIR/component_keyring_vault.so" ]`) {
		t.Errorf("the chosen plugin_dir is not checked for the component:\n%s", mysqlKeyringStageComponentScript)
	}
}

// The verification step is the one that catches a keyring that loaded but cannot reach OpenBao —
// a wrong mount or a dead token looks exactly like a healthy one until the first key.
func TestMySQLKeyringVerifyScriptProvesTheKeyRoundTrips(t *testing.T) {
	for _, want := range []string{
		"performance_schema.keyring_component_status", // the component's status
		"information_schema.plugins",                  // the plugin's
		`ENCRYPTION='Y'`,                              // and the end-to-end proof
	} {
		if !strings.Contains(mysqlKeyringVerifyScript, want) {
			t.Errorf("the verify script does not check %q:\n%s", want, mysqlKeyringVerifyScript)
		}
	}
	// PXC runs pxc_strict_mode=ENFORCING, which refuses a table with no primary key — so the
	// probe table would fail on a cluster for a reason that has nothing to do with the keyring.
	if !strings.Contains(mysqlKeyringVerifyScript, "id INT PRIMARY KEY") {
		t.Errorf("the probe table needs a primary key or PXC strict mode refuses it:\n%s", mysqlKeyringVerifyScript)
	}
	// And it cleans up after itself: a lab left with a stray database is a lab that makes
	// somebody wonder what wrote it.
	if !strings.Contains(mysqlKeyringVerifyScript, "DROP DATABASE dbcanvas_tde_check") {
		t.Errorf("the probe must drop what it created:\n%s", mysqlKeyringVerifyScript)
	}
}

// Every server gets its own mount, cluster member or not — Percona: "each secret_mount_point
// must serve only one Percona Server instance".
func TestMySQLVaultMountIsPerServer(t *testing.T) {
	seen := map[string]string{}
	for _, host := range []string{"pxc01", "pxc02", "pxc03"} {
		mount, kv, ver := mysqlVaultMount("8.4", host)
		if prev, dup := seen[mount]; dup {
			t.Fatalf("%s and %s share the mount %s", prev, host, mount)
		}
		seen[mount] = host
		if mount != "mysql-"+host || kv != "kv-v2" || ver != "2" {
			t.Errorf("mount for %s = (%s, %s, v%s)", host, mount, kv, ver)
		}
	}
	// 5.7's plugin predates the KV v2 API.
	if _, kv, ver := mysqlVaultMount("5.7", "ps01"); kv != "kv" || ver != "1" {
		t.Errorf("5.7 got (%s, v%s), want (kv, v1)", kv, ver)
	}
}

// The options a node's own config must carry. The plugin needs loading; the component is
// declared by a manifest and needs nothing in my.cnf — but both encrypt new tables by default,
// which is what "this cluster is encrypted" has to mean to somebody creating a table on it.
func TestMySQLVaultOptions(t *testing.T) {
	plugin := mysqlVaultOptions("8.0")
	if !strings.Contains(plugin, "early-plugin-load=keyring_vault.so") ||
		!strings.Contains(plugin, "keyring_vault_config="+mysqlKeyringConf) {
		t.Errorf("8.0 must load the plugin early:\n%s", plugin)
	}
	component := mysqlVaultOptions("8.4")
	if strings.Contains(component, "early-plugin-load") {
		t.Errorf("8.4 has no keyring plugin to load — it is a component:\n%s", component)
	}
	for _, o := range []string{plugin, component} {
		if !strings.Contains(o, "default_table_encryption=ON") {
			t.Errorf("a keyed node should encrypt new tables by default:\n%s", o)
		}
	}
	// A node with no keyring gets no options at all, and nothing in the config changes.
	if got := mysqlMyCnf(designFrame{PSMajor: "8.0"}, "ps01", ""); strings.Contains(got, "default_table_encryption") {
		t.Errorf("an unencrypted node must not be given encryption options:\n%s", got)
	}
}

// The options have to reach the file the server actually reads, which is the node's own
// rendered config — not an append to /etc/my.cnf, which Debian-family packages never read.
func TestKeyringOptionsLandInTheNodeConfig(t *testing.T) {
	opts := mysqlVaultOptions("8.0")
	pxc := pxcMyCnf(designFrame{Type: "pxc", PXCMajor: "8.0", Label: "c1"}, designNode{}, "pxc01", "example.net", "pxc01.example.net", opts)
	if !strings.Contains(pxc, "early-plugin-load=keyring_vault.so") {
		t.Errorf("the PXC config lost its keyring options:\n%s", pxc)
	}
	// SST must stay on xtrabackup-v2: PXC aborts an rsync SST outright when a vault keyring is
	// configured, because rsync cannot do the re-encryption step.
	if !strings.Contains(pxc, "wsrep_sst_method=xtrabackup-v2") {
		t.Errorf("a keyed PXC member must not SST with rsync:\n%s", pxc)
	}
	repl := mysqlMyCnf(designFrame{Type: "mysql", PSMajor: "8.0"}, "ps01", opts)
	if !strings.Contains(repl, "early-plugin-load=keyring_vault.so") {
		t.Errorf("the replication config lost its keyring options:\n%s", repl)
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

// A cluster keyed to OpenBao has to be linked to one that exists — the same check a standalone
// node gets, on the frame that carries the flag for every member.
func TestVaultFrameIssues(t *testing.T) {
	bao := map[string]bool{"bao1": true}
	for _, ft := range []string{"pxc", "mysql"} {
		if got := vaultFrameIssues(designFrame{Type: ft, Label: "c1", EnableVault: true, OpenBaoNodeID: "bao1"}, bao); len(got) != 0 {
			t.Errorf("%s: a linked cluster reported %v", ft, got)
		}
		if got := vaultFrameIssues(designFrame{Type: ft, Label: "c1", EnableVault: true}, bao); len(got) == 0 {
			t.Errorf("%s: encryption with no OpenBao node was accepted", ft)
		}
		if got := vaultFrameIssues(designFrame{Type: ft, Label: "c1", EnableVault: true, OpenBaoNodeID: "gone"}, bao); len(got) == 0 {
			t.Errorf("%s: a dangling OpenBao reference was accepted", ft)
		}
		if got := vaultFrameIssues(designFrame{Type: ft, Label: "c1"}, bao); len(got) != 0 {
			t.Errorf("%s: an unencrypted cluster reported %v", ft, got)
		}
	}
	// keyring_vault is a Percona Server component: MariaDB's equivalent is a different plugin
	// with a different key format, and the MySQL Community packages have none at all. A flag set
	// on one of those frames is not an error to report here — it is a frame that never offers it.
	for _, ft := range []string{"mariadbgalera", "mysqlcerepl", "innodb", "patroni"} {
		if got := vaultFrameIssues(designFrame{Type: ft, Label: "c1", EnableVault: true}, bao); len(got) != 0 {
			t.Errorf("%s is not a vault-capable frame but reported %v", ft, got)
		}
	}
}

// An encrypted PXC cluster must encrypt its cluster traffic, and this is not a preference: PXC's
// own SST script refuses to run without it once a keyring is configured —
//
//	FATAL: keyring component is enabled but transit channel is unencrypted.
//	Enable encryption for SST traffic
//
// which was found by deploying: the first member bootstrapped, and the second could never join.
func TestEncryptedPXCTurnsOnClusterTraffic(t *testing.T) {
	frame := designFrame{Type: "pxc", PXCMajor: "8.4", Label: "c1", EnableVault: true}
	cnf := pxcMyCnf(frame, designNode{}, "pxc01", "example.net", "pxc01.example.net", mysqlVaultOptions("8.4"))
	if !strings.Contains(cnf, "pxc_encrypt_cluster_traffic=ON") {
		t.Errorf("a keyed cluster left its transit channel unencrypted — SST will refuse:\n%s", cnf)
	}
	// The certificate is one file set shared by every member, staged outside the data directory:
	// a joiner's datadir is empty before its first start and replaced wholesale by SST, and these
	// files have to exist before either happens.
	for _, want := range []string{
		"ssl-ca=" + pxcSSLDir + "/ca.pem",
		"ssl-cert=" + pxcSSLDir + "/server-cert.pem",
		"ssl-key=" + pxcSSLDir + "/server-key.pem",
		"[sst]", "encrypt=4",
	} {
		if !strings.Contains(cnf, want) {
			t.Errorf("the config is missing %q:\n%s", want, cnf)
		}
	}
	// [sst] ends [mysqld], so nothing the server needs may come after it — the keyring options
	// in particular, which would silently become SST options.
	sst := strings.Index(cnf, "[sst]")
	if k := strings.Index(cnf, "default_table_encryption"); k > sst {
		t.Error("a server option was written after [sst], where the server will never read it")
	}

	// And an unencrypted cluster is left exactly as it was: no TLS, no [sst] section.
	plain := pxcMyCnf(designFrame{Type: "pxc", PXCMajor: "8.4", Label: "c1"}, designNode{}, "pxc01", "example.net", "pxc01.example.net", "")
	if !strings.Contains(plain, "pxc_encrypt_cluster_traffic=OFF") || strings.Contains(plain, "[sst]") {
		t.Errorf("an unencrypted cluster's config changed:\n%s", plain)
	}
}

// The shared material is staged where a joiner's SST cannot delete it, readable by mysqld and by
// nobody else.
func TestClusterSSLStagingIsOutsideTheDatadir(t *testing.T) {
	if strings.HasPrefix(pxcSSLDir, "/var/lib/mysql") {
		t.Fatalf("cluster TLS material lives in %s — a joiner's SST replaces the datadir", pxcSSLDir)
	}
	for _, want := range []string{
		"chown mysql:mysql " + pxcSSLDir + "/*.pem",
		"chmod 0640 " + pxcSSLDir + "/server-key.pem",
	} {
		if !strings.Contains(pxcClusterSSLScript, want) {
			t.Errorf("the staging script is missing %q:\n%s", want, pxcClusterSSLScript)
		}
	}
	// The CA's private key never travels: the certificate is signed on the node that owns it.
	if strings.Contains(pxcClusterSSLScript, "ca.key") {
		t.Errorf("the CA private key must not be staged on a database node:\n%s", pxcClusterSSLScript)
	}
	if !strings.Contains(pxcMintClusterSSLScript, "/etc/pki/dbcanvas/ca.key") {
		t.Errorf("the cluster certificate must be signed from the Intranet CA:\n%s", pxcMintClusterSSLScript)
	}
}
