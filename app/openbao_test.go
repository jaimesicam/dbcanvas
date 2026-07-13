package main

import (
	"strings"
	"testing"
)

// The TLS material must be named in openbao.hcl from /etc/openbao.d/tls, and api_addr must be
// the node's FQDN (a client redirected by the API has to land on a name the stack's DNS knows).
func TestOpenBaoHCL(t *testing.T) {
	hcl := openbaoHCL("bao01.example.net", true)
	for _, want := range []string{
		`tls_cert_file = "/etc/openbao.d/tls/server.crt"`,
		`tls_key_file  = "/etc/openbao.d/tls/server.key"`,
		`tls_client_ca_file = "/etc/openbao.d/tls/ca.crt"`,
		`address = "0.0.0.0:8200"`,
		`api_addr = "https://bao01.example.net:8200"`,
		`path = "/opt/openbao/data"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("TLS openbao.hcl missing %q\n%s", want, hcl)
		}
	}
	if strings.Contains(hcl, "tls_disable") {
		t.Errorf("TLS openbao.hcl must not disable TLS:\n%s", hcl)
	}

	plain := openbaoHCL("bao01.example.net", false)
	if !strings.Contains(plain, "tls_disable = 1") {
		t.Errorf("non-TLS openbao.hcl should set tls_disable:\n%s", plain)
	}
	if strings.Contains(plain, "tls_cert_file") {
		t.Errorf("non-TLS openbao.hcl must not name a certificate:\n%s", plain)
	}
	if !strings.Contains(plain, `api_addr = "http://bao01.example.net:8200"`) {
		t.Errorf("non-TLS api_addr should be http:\n%s", plain)
	}
}

// VAULT_ADDR is the node's FQDN and VAULT_CACERT the Intranet CA copy — the two variables the
// Percona engines and the bao CLI read.
func TestOpenBaoProfile(t *testing.T) {
	p := openbaoProfile(openbaoAddr("bao01.example.net", true), true)
	for _, want := range []string{
		`export VAULT_ADDR="https://bao01.example.net:8200"`,
		`export VAULT_CACERT="/etc/openbao.d/tls/ca.crt"`,
		`export BAO_ADDR="https://bao01.example.net:8200"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q\n%s", want, p)
		}
	}
	if plain := openbaoProfile(openbaoAddr("bao01.example.net", false), false); strings.Contains(plain, "CACERT") {
		t.Errorf("a non-TLS node has no CA to verify:\n%s", plain)
	}
}

// Policies: KV v1 is a flat tree; KV v2 splits data/metadata and PSMDB additionally reads
// <mount>/config and the metadata (Percona's documented policy).
func TestOpenBaoPolicy(t *testing.T) {
	v1 := openbaoPolicy("mysql-v1", "kv", "Percona Server for MySQL")
	if !strings.Contains(v1, `path "mysql-v1/*"`) || strings.Contains(v1, "/data/") {
		t.Errorf("KV v1 policy should be a flat mount rule:\n%s", v1)
	}

	mongo := openbaoPolicy("mongodb-v2", "kv-v2", "Percona Server for MongoDB")
	for _, want := range []string{
		`path "mongodb-v2/data/*"`,
		`path "mongodb-v2/metadata/*"`,
		`path "mongodb-v2/config"`,
	} {
		if !strings.Contains(mongo, want) {
			t.Errorf("PSMDB v2 policy missing %q\n%s", want, mongo)
		}
	}
	// PSMDB only ever reads metadata (it checks the version count before rotating a key).
	for _, line := range strings.Split(mongo, "\n") {
		if strings.Contains(line, "metadata") {
			continue
		}
	}
	if !strings.Contains(mongo, "path \"mongodb-v2/metadata/*\" {\n  capabilities = [\"read\"]") {
		t.Errorf("PSMDB metadata should be read-only:\n%s", mongo)
	}

	// The MySQL keyring component writes keys, so it needs write access to both trees.
	mysql := openbaoPolicy("mysql-v2", "kv-v2", "Percona Server for MySQL")
	if !strings.Contains(mysql, "path \"mysql-v2/metadata/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\", \"list\"]") {
		t.Errorf("MySQL v2 metadata should be writable:\n%s", mysql)
	}
}

// Every mount is offered with a policy file next to the server config. MySQL gets a v1 and a v2
// mount (keyring_vault speaks both); MongoDB gets v2 only — PSMDB cannot use KV v1 at all, so a
// mongodb-v1 mount would just be a trap.
func TestOpenBaoMounts(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range openbaoMounts {
		seen[m.Path] = true
		if m.KV != "kv" && m.KV != "kv-v2" {
			t.Errorf("mount %s: unknown KV engine %q", m.Path, m.KV)
		}
		if p := openbaoPolicy(m.Path, m.KV, m.Engine); !strings.Contains(p, m.Path) {
			t.Errorf("mount %s: policy does not scope to the mount", m.Path)
		}
		if strings.Contains(m.Engine, "MongoDB") && m.KV != "kv-v2" {
			t.Errorf("mount %s: PSMDB only supports KV v2, got %q", m.Path, m.KV)
		}
	}
	for _, want := range []string{"mysql-v1", "mysql-v2", "mongodb-v2"} {
		if !seen[want] {
			t.Errorf("missing KV mount %q", want)
		}
	}
	if seen["mongodb-v1"] {
		t.Error("mongodb-v1 must not exist: PSMDB supports KV v2 only")
	}
	if len(openbaoMounts) != 3 {
		t.Errorf("expected 3 KV mounts, got %d", len(openbaoMounts))
	}
}
