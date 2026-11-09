package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestPGCustomTLSManifest(t *testing.T) {
	m := pgCustomTLSManifest("pg-01", "db", "example.net", k3dReplCAIssuer)
	docs := strings.Split(m, "---\n")
	if len(docs) != 3 {
		t.Fatalf("want 3 Certificates, got %d:\n%s", len(docs), m)
	}
	type cert struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			SecretName string   `json:"secretName"`
			CommonName string   `json:"commonName"`
			DNSNames   []string `json:"dnsNames"`
			IssuerRef  struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"issuerRef"`
			Usages []string `json:"usages"`
		} `json:"spec"`
	}
	got := map[string]cert{}
	for _, d := range docs {
		var c cert
		if err := yaml.Unmarshal([]byte(d), &c); err != nil {
			t.Fatalf("not YAML: %v\n%s", err, d)
		}
		if c.Kind != "Certificate" || c.Metadata.Namespace != "db" || c.Spec.SecretName != c.Metadata.Name ||
			c.Spec.IssuerRef.Name != k3dReplCAIssuer || c.Spec.IssuerRef.Kind != "Issuer" {
			t.Errorf("bad Certificate: %+v", c)
		}
		got[c.Metadata.Name] = c
	}
	n := pgCustomTLSNames("pg-01")
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	server := got[n.Server]
	for _, want := range []string{"pg-01-primary", "pg-01-primary.db.svc", "pg-01-replicas.db.svc.cluster.local", "pg-01-ha.db",
		"pg-01-ha.example.net", "pg-01-replicas.example.net"} {
		if !has(server.Spec.DNSNames, want) {
			t.Errorf("server certificate lacks %s: %v", want, server.Spec.DNSNames)
		}
	}
	if !has(server.Spec.Usages, "server auth") {
		t.Errorf("server certificate usages: %v", server.Spec.Usages)
	}
	repl := got[n.Replication]
	if repl.Spec.CommonName != "_crunchyrepl" || !has(repl.Spec.Usages, "client auth") || has(repl.Spec.Usages, "server auth") {
		t.Errorf("replication certificate: %+v", repl.Spec)
	}
	bouncer := got[n.PGBouncer]
	if !has(bouncer.Spec.DNSNames, "pg-01-pgbouncer.example.net") || !has(bouncer.Spec.DNSNames, "pg-01-pgbouncer.db.svc") {
		t.Errorf("pgBouncer certificate names: %v", bouncer.Spec.DNSNames)
	}
}

// The issuer and the custom certificates in the operator's real cr.yaml (3.1.0), parsed back.
func TestPGTransformIssuerAndCustomTLS(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	n := pgCustomTLSNames("pg-01")
	out := pgTransform(string(raw), pgOptions{Name: "pg-01", ExposePGBouncer: "LoadBalancer", CustomTLS: &n})
	cfg := &k3dConfig{OperatorVer: "3.1.0"}
	out = k3dCRIssuer(out, string(raw), k3dReplCAIssuer, nil, cfg, func(string) {})

	var cr struct {
		Spec struct {
			TLS struct {
				IssuerConf struct {
					Name  string `json:"name"`
					Kind  string `json:"kind"`
					Group string `json:"group"`
				} `json:"issuerConf"`
			} `json:"tls"`
			Secrets struct {
				CustomTLSSecret struct {
					Name string `json:"name"`
				} `json:"customTLSSecret"`
				CustomReplicationTLSSecret struct {
					Name string `json:"name"`
				} `json:"customReplicationTLSSecret"`
			} `json:"secrets"`
			Proxy struct {
				PGBouncer struct {
					Expose struct {
						Type string `json:"type"`
					} `json:"expose"`
					CustomTLSSecret struct {
						Name string `json:"name"`
					} `json:"customTLSSecret"`
				} `json:"pgBouncer"`
			} `json:"proxy"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &cr); err != nil {
		t.Fatalf("rewritten cr.yaml is not YAML: %v", err)
	}
	if ic := cr.Spec.TLS.IssuerConf; ic.Name != k3dReplCAIssuer || ic.Kind != "Issuer" || ic.Group != "cert-manager.io" {
		t.Errorf("tls.issuerConf = %+v", ic)
	}
	if cr.Spec.Secrets.CustomTLSSecret.Name != n.Server || cr.Spec.Secrets.CustomReplicationTLSSecret.Name != n.Replication {
		t.Errorf("spec.secrets = %+v", cr.Spec.Secrets)
	}
	if pb := cr.Spec.Proxy.PGBouncer; pb.CustomTLSSecret.Name != n.PGBouncer || pb.Expose.Type != "LoadBalancer" {
		t.Errorf("proxy.pgBouncer = %+v", pb)
	}
	// Without either, nothing of the kind is written.
	plain := pgTransform(string(raw), pgOptions{Name: "pg-01"})
	if strings.Contains(plain, "\n  secrets:") || strings.Contains(plain, "\n      customTLSSecret:") {
		t.Error("custom TLS secrets written without being asked for")
	}
}

func TestPGTLSHostCandidates(t *testing.T) {
	got := strings.Join(k3dTLSHostCandidates("pg", "pg-01", false), ",")
	if got != "pg-01-ha,pg-01-replicas,pg-01-pgbouncer" {
		t.Errorf("pg candidates = %s", got)
	}
}
