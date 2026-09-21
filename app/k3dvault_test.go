package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The PXC half runs against the operator's real cr.yaml, because the thing worth testing is
// whether the *shipped* commented-out `vaultSecretName` line is the one that gets turned on.
func TestCRTransformVaultSecret(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no PXC cr.yaml fixture: %v", err)
	}
	out := crTransform(string(raw), crOptions{Name: "pxc-01", VaultSecret: "pxc-01-vault"})

	// Exactly one active vaultSecretName, at spec level, naming our Secret. Two would be a map
	// with a duplicate key — the failure crDuplicateWarning exists for.
	active := 0
	for i, ln := range strings.Split(out, "\n") {
		ind, commented, body := crLine(ln)
		if commented || !strings.HasPrefix(body, "vaultSecretName:") {
			continue
		}
		active++
		if ind != 2 {
			t.Errorf("line %d: vaultSecretName must be a spec-level key, found at indent %d", i+1, ind)
		}
		if body != "vaultSecretName: pxc-01-vault" {
			t.Errorf("line %d: %q", i+1, body)
		}
	}
	if active != 1 {
		t.Fatalf("want exactly 1 active vaultSecretName, got %d", active)
	}

	// And a frame that did not ask for encryption leaves the line as Percona ships it — commented
	// out. Anything else would name a Secret that does not exist.
	plain := crTransform(string(raw), crOptions{Name: "pxc-01"})
	for i, ln := range strings.Split(plain, "\n") {
		_, commented, body := crLine(ln)
		if !commented && strings.HasPrefix(body, "vaultSecretName:") {
			t.Errorf("line %d: vaultSecretName active on an unencrypted cluster: %q", i+1, body)
		}
	}
}

// PSMDB's encryption is two halves that have to agree: `spec.secrets.vault` names the Secret, and
// each data-bearing replica set's `configuration` carries the mongod `security.vault` block. The
// operator keys off both — a CR with only one of them deploys a cluster that is not encrypted.
func TestPSMDBTransformVault(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-psmdb.yaml")
	if err != nil {
		t.Skipf("no PSMDB cr.yaml fixture: %v", err)
	}
	v := &k3dVault{
		Secret: "mongo-01-vault", Mount: "mongodb-mongo-01",
		VaultHost: "https://bao-01.example.net:8200", VaultFQDN: "bao-01.example.net", TLS: true,
	}
	out := psmdbTransform(string(raw), psmdbOptions{Name: "mongo-01", Sharding: true, Vault: v})

	if !strings.Contains(out, "\n    vault: mongo-01-vault\n") {
		t.Error("spec.secrets.vault was not written — the operator would mount no credentials")
	}

	// Both replica sets get a block, and each gets its OWN key path: rs0 and the config servers
	// are separate WiredTiger deployments, and sharing one path has the second to start
	// overwrite the first's master key.
	for _, want := range []string{
		"secret: mongodb-mongo-01/data/mongo-01-rs0",
		"secret: mongodb-mongo-01/data/mongo-01-cfg",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(out, "enableEncryption: true"); n != 2 {
		t.Errorf("want 2 encrypted replica sets (rs0 + config servers), got %d", n)
	}
	// TLS on → verify OpenBao with the CA out of the same Secret, and never the testing escape.
	if !strings.Contains(out, "serverCAFile: /etc/mongodb-vault/ca.crt") {
		t.Error("a TLS OpenBao must be verified with serverCAFile")
	}
	if strings.Contains(out, "disableTLSForTesting") {
		t.Error("disableTLSForTesting must not appear when OpenBao serves TLS")
	}
	if !strings.Contains(out, "tokenFile: /etc/mongodb-vault/token") {
		t.Error("the token file must be the path the operator mounts the Secret at")
	}

	// A plain replica set encrypts rs0 only — there are no config servers to key.
	plain := psmdbTransform(string(raw), psmdbOptions{Name: "mongo-01", Vault: v})
	if n := strings.Count(plain, "enableEncryption: true"); n != 1 {
		t.Errorf("an unsharded cluster must encrypt one replica set, got %d", n)
	}
	if strings.Contains(plain, "mongo-01-cfg") {
		t.Error("an unsharded cluster has no config servers to key")
	}

	// And no vault → nothing written at all, rather than a half-configured CR.
	none := psmdbTransform(string(raw), psmdbOptions{Name: "mongo-01", Sharding: true})
	for _, unwanted := range []string{"enableEncryption: true", "vault: mongo-01-vault"} {
		if strings.Contains(none, unwanted) {
			t.Errorf("an unencrypted cluster must not carry %q", unwanted)
		}
	}
}

// cr.yaml documents `secrets.vault` as a commented line a few rows below the one this transform
// writes, and uncommenting it is the obvious thing to do when reading the file in /root — which
// would put two `vault` keys in one map. It gets the same warning crTransform writes.
func TestPSMDBVaultMarksTheShippedDuplicate(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-psmdb.yaml")
	if err != nil {
		t.Skipf("no PSMDB cr.yaml fixture: %v", err)
	}
	v := &k3dVault{Secret: "mongo-01-vault", Mount: "mongodb-mongo-01", VaultFQDN: "bao-01.example.net"}
	out := psmdbTransform(string(raw), psmdbOptions{Name: "mongo-01", Vault: v})

	lines := strings.Split(out, "\n")
	warned := false
	for i, ln := range lines {
		_, commented, body := crLine(ln)
		if !commented || !strings.HasPrefix(body, "vault: my-cluster-name-vault") {
			continue
		}
		if i > 0 && strings.Contains(lines[i-1], "edit the one above instead") {
			warned = true
		}
	}
	if !warned {
		t.Error("the shipped commented-out `vault:` must carry the duplicate-key warning")
	}
	// The warning is a comment, so it must not add an active key of its own.
	active := 0
	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		if !commented && ind == 4 && strings.HasPrefix(body, "vault:") {
			active++
		}
	}
	if active != 1 {
		t.Errorf("want exactly 1 active secrets.vault, got %d", active)
	}
}

// A plaintext OpenBao is the one case where mongod is told not to verify anything, and it has to
// be spelled out rather than left to a missing serverCAFile (which mongod treats as "use the
// system store" and then fails to find a self-signed lab CA in it).
func TestPSMDBVaultConfigurationWithoutTLS(t *testing.T) {
	v := &k3dVault{Secret: "s", Mount: "mongodb-m", VaultFQDN: "bao-01.example.net", TLS: false}
	got := psmdbVaultConfiguration(v, "m-rs0")
	if !strings.Contains(got, "disableTLSForTesting: true") {
		t.Errorf("a plaintext OpenBao needs disableTLSForTesting:\n%s", got)
	}
	if strings.Contains(got, "serverCAFile") {
		t.Errorf("there is no CA to verify a plaintext listener with:\n%s", got)
	}
	if !strings.Contains(got, "port: 8200") {
		t.Errorf("mongod dials OpenBao's API port:\n%s", got)
	}
}

// The credentials Secret carries the CA only when there is TLS to verify — a caSecret key the
// Secret does not hold stops the pods from starting, which is the pg_tde lesson applied here.
func TestK3DVaultSecret(t *testing.T) {
	pxc := k3dVaultSecret("pxc-01-vault", map[string]string{
		pxcVaultConfKey: "vault_url = https://bao-01.example.net:8200\nsecret_mount_point = mysql-pxc-01\ntoken = hvs.abc\n",
	}, "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	for _, want := range []string{
		"  name: pxc-01-vault",
		"  keyring_vault.conf: |-",
		"    vault_url = https://bao-01.example.net:8200",
		"  ca.crt: |",
		"    -----BEGIN CERTIFICATE-----",
	} {
		if !strings.Contains(pxc, want) {
			t.Errorf("missing %q in:\n%s", want, pxc)
		}
	}

	// A single-line value stays a scalar, and no CA means no ca.crt key at all.
	mongo := k3dVaultSecret("mongo-01-vault", map[string]string{psmdbVaultTokenKey: "hvs.abc"}, "")
	if !strings.Contains(mongo, "\n  token: hvs.abc\n") {
		t.Errorf("the token should be a plain scalar:\n%s", mongo)
	}
	if strings.Contains(mongo, "ca.crt") {
		t.Errorf("a plaintext OpenBao must leave ca.crt out:\n%s", mongo)
	}
}

// The keyring config format depends on the SERVER version, not the operator: the operator's
// entrypoint feeds the same Secret file to the keyring_vault plugin on 5.7/8.0 and copies it
// verbatim to the 8.4 component's config, which parses it as JSON. Writing the plugin's
// `key = value` file on 8.4 crash-loops every database pod, which is how this was found.
func TestPXCKeyringConfFollowsTheServerVersion(t *testing.T) {
	ca := pxcVaultMountPath + "/" + k3dVaultCAKey

	// 8.0 and older: the plugin's key = value file.
	plugin := mysqlKeyringPluginConf("https://bao-01.example.net:8200", "mysql-pxc-01", "hvs.abc", ca, "2")
	for _, want := range []string{
		"vault_url = https://bao-01.example.net:8200",
		"secret_mount_point = mysql-pxc-01",
		"token = hvs.abc",
		"vault_ca = /etc/mysql/vault-keyring-secret/ca.crt",
		"secret_mount_point_version = 2",
	} {
		if !strings.Contains(plugin, want) {
			t.Errorf("plugin config missing %q in:\n%s", want, plugin)
		}
	}

	// 8.4: the component's JSON. It must actually parse — the server's error for a malformed
	// one is "Keyring configuration JSON parse error", after which InnoDB refuses to start.
	comp := mysqlKeyringComponentConf("https://bao-01.example.net:8200", "mysql-pxc-01", "hvs.abc", ca)
	var got map[string]any
	if err := json.Unmarshal([]byte(comp), &got); err != nil {
		t.Fatalf("the 8.4 component config must be valid JSON: %v\n%s", err, comp)
	}
	for k, want := range map[string]any{
		"vault_url":          "https://bao-01.example.net:8200",
		"secret_mount_point": "mysql-pxc-01",
		"token":              "hvs.abc",
		"vault_ca":           ca,
	} {
		if got[k] != want {
			t.Errorf("component config %s = %v, want %v", k, got[k], want)
		}
	}

	// And the predicate that chooses between them is the one the rest of the codebase uses.
	if !mysqlModernMajor("8.4") {
		t.Error("8.4 must select the component")
	}
	if mysqlModernMajor("8.0") || mysqlModernMajor("5.7") {
		t.Error("8.0 and 5.7 must select the plugin")
	}
}

// The server version comes from the `spec.pxc.image` tag in the operator release's own cr.yaml —
// the only place in the deploy that states which database this cluster will run.
func TestCRPXCImageMajor(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no PXC cr.yaml fixture: %v", err)
	}
	if got := crPXCImageMajor(string(raw)); got != "8.4" {
		t.Errorf("cr.yaml 1.20.0 pins percona-xtradb-cluster:8.4.8-8.1, got major %q", got)
	}

	// It must read the pxc section's own image, not the first one in the file, and not a
	// commented-out example. cr.yaml carries a dozen image lines.
	src := strings.Join([]string{
		"spec:",
		"  crVersion: 1.20.0",
		"#  pxc:",
		"#    image: percona/percona-xtradb-cluster:5.7.44-31.65",
		"  haproxy:",
		"    image: percona/haproxy:2.8.15",
		"  pxc:",
		"    size: 3",
		"    image: percona/percona-xtradb-cluster:8.0.43-34.1",
		"  proxysql:",
		"    image: percona/proxysql2:2.7.3",
	}, "\n")
	if got := crPXCImageMajor(src); got != "8.0" {
		t.Errorf("want the pxc section's own image (8.0), got %q", got)
	}

	// No readable image → "", which the caller turns into a refused deploy rather than a guess.
	if got := crPXCImageMajor("spec:\n  pxc:\n    size: 3\n"); got != "" {
		t.Errorf("an unreadable image must be empty, got %q", got)
	}
}

func TestK3DVaultMountAndGate(t *testing.T) {
	if got := k3dVaultMount("pxc", "pxc-01"); got != "mysql-pxc-01" {
		t.Errorf("k3dVaultMount(pxc) = %q", got)
	}
	if got := k3dVaultMount("psmdb", "mongo-01"); got != "mongodb-mongo-01" {
		t.Errorf("k3dVaultMount(psmdb) = %q", got)
	}
	for _, tc := range []struct {
		ver  string
		want bool
	}{
		{"1.23.0", true}, {"1.13.0", true}, {"1.12.0", false}, {"1.9.0", false},
		{"", false}, // unknown is not "newest"
	} {
		if got := psmdbHasVault(tc.ver); got != tc.want {
			t.Errorf("psmdbHasVault(%q) = %v, want %v", tc.ver, got, tc.want)
		}
	}
	// The toggle only means anything on the two operators that have a vault integration.
	for op, want := range map[string]bool{"pxc": true, "psmdb": true, "pg": false, "ps": false, "cnpg": false, "": false} {
		f := designFrame{Type: "k3d", K3DOperator: op, K3DVaultEncryption: true}
		if got := k3dVaultOn(f); got != want {
			t.Errorf("k3dVaultOn(%q) = %v, want %v", op, got, want)
		}
	}
}

func TestK3DVaultIssues(t *testing.T) {
	cat := OperatorCatalog{
		"psmdb": {Latest: "1.23.0", Versions: []string{"1.23.0", "1.13.0", "1.12.0"}},
		"pxc":   {Latest: "1.20.0", Versions: []string{"1.20.0", "1.7.0"}},
	}
	bao := designDoc{Nodes: []designNode{{ID: "b1", Type: "openbao", Label: "bao-01"}}}
	empty := designDoc{}

	// Off → silent.
	off := designFrame{Type: "k3d", Label: "k3d-00", K3DOperator: "pxc", K3DOperatorVer: "1.20.0"}
	if iss := k3dVaultIssues(off, empty, cat, false); len(iss) != 0 {
		t.Fatalf("an unencrypted frame must be silent, got %v", iss)
	}

	// On, with a key store → silent. PXC has had vaultSecretName since the oldest version the
	// catalog carries, so there is no version gate on that side.
	on := off
	on.K3DVaultEncryption, on.OpenBaoNodeID = true, "b1"
	if iss := k3dVaultIssues(on, bao, cat, false); len(iss) != 0 {
		t.Fatalf("PXC with an OpenBao node must be silent, got %v", iss)
	}
	oldPXC := on
	oldPXC.K3DOperatorVer = "1.7.0"
	if iss := k3dVaultIssues(oldPXC, bao, cat, false); len(iss) != 0 {
		t.Fatalf("PXC 1.7.0 has vaultSecretName; want silence, got %v", iss)
	}

	// On, with nothing to key it to → error, because there is no local keyring to fall back on.
	noBao := on
	iss := k3dVaultIssues(noBao, empty, cat, false)
	if len(iss) != 1 || iss[0].Level != "error" {
		t.Fatalf("encryption without an OpenBao node must be an error, got %v", iss)
	}
	if !strings.Contains(iss[0].Message, "OpenBao") {
		t.Errorf("the error must name what is missing: %q", iss[0].Message)
	}
	// …and a warning rather than an error once the frame is running: nothing re-applies its
	// cr.yaml, so an error would only block unrelated work on the same canvas.
	if iss := k3dVaultIssues(noBao, empty, cat, true); len(iss) != 1 || iss[0].Level != "warning" {
		t.Errorf("a deployed frame must warn, not error: %v", iss)
	}

	// PSMDB below 1.13.0 has no secrets.vault. This is the dangerous case — the cluster would
	// come up healthy and unencrypted — so it is an error naming both versions.
	oldMongo := designFrame{Type: "k3d", Label: "k3d-01", K3DOperator: "psmdb",
		K3DOperatorVer: "1.12.0", K3DVaultEncryption: true, OpenBaoNodeID: "b1"}
	iss = k3dVaultIssues(oldMongo, bao, cat, false)
	if len(iss) != 1 || iss[0].Level != "error" {
		t.Fatalf("PSMDB 1.12.0 must be an error, got %v", iss)
	}
	if !strings.Contains(iss[0].Message, "1.12.0") || !strings.Contains(iss[0].Message, psmdbVaultMinVer) {
		t.Errorf("the error must name both versions: %q", iss[0].Message)
	}
	newMongo := oldMongo
	newMongo.K3DOperatorVer = "1.13.0"
	if iss := k3dVaultIssues(newMongo, bao, cat, false); len(iss) != 0 {
		t.Errorf("PSMDB 1.13.0 must be accepted, got %v", iss)
	}

	// An operator with no vault integration ignores the setting — a warning, not an error, and
	// PostgreSQL is pointed at the option it does have.
	wrong := on
	wrong.K3DOperator = "pg"
	iss = k3dVaultIssues(wrong, bao, cat, false)
	if len(iss) != 1 || iss[0].Level != "warning" {
		t.Fatalf("a vault-less operator must warn, got %v", iss)
	}
	if !strings.Contains(iss[0].Message, "pg_tde") {
		t.Errorf("a PostgreSQL frame should be pointed at pg_tde: %q", iss[0].Message)
	}
}
