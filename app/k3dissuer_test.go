package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The shapes cr.yaml ships `tls:` in, per operator and release.
const (
	crPXCLiveTLS = `apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBCluster
metadata:
  name: cluster1
spec:
  crVersion: 1.20.0
  tls:
    enabled: true
#    SANs:
#      - pxc-1.example.com
#    issuerConf:
#      name: special-selfsigned-issuer
#      kind: ClusterIssuer
#      group: cert-manager.io
  pxc:
    size: 3`
	crPXCCommentedTLS = `apiVersion: pxc.percona.com/v1-7-0
kind: PerconaXtraDBCluster
metadata:
  name: cluster1
spec:
  crVersion: 1.7.0
#  tls:
#    SANs:
#      - pxc-1.example.com
#    issuerConf:
#      name: special-selfsigned-issuer
#      kind: ClusterIssuer
#      group: cert-manager.io
  pxc:
    size: 3`
)

func crSpecTLS(t *testing.T, cr string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(cr), &doc); err != nil {
		t.Fatalf("the result is not valid YAML: %v\n%s", err, cr)
	}
	tls, _ := doc["spec"].(map[string]any)["tls"].(map[string]any)
	return tls
}

func TestCRTLSSectionIssuer(t *testing.T) {
	want := map[string]any{"name": "dbcanvas-openbao", "kind": "Issuer", "group": "cert-manager.io"}
	check := func(name string, tls map[string]any) {
		t.Helper()
		got, _ := tls["issuerConf"].(map[string]any)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: issuerConf.%s = %v, want %v (tls = %v)", name, k, got[k], v, tls)
			}
		}
	}

	// PXC 1.15.0+: the section is live, so issuerConf goes into it and `enabled` survives.
	live := crSpecTLS(t, crTLSSection(crPXCLiveTLS, "dbcanvas-openbao", nil))
	check("live tls", live)
	if live["enabled"] != true {
		t.Errorf("live tls lost enabled: %v", live)
	}

	// Older PXC: the whole section is commented out, so one is created above the example.
	old := crTLSSection(crPXCCommentedTLS, "dbcanvas-openbao", nil)
	check("commented tls", crSpecTLS(t, old))
	if !strings.Contains(old, "  tls:\n    issuerConf:\n") || strings.Index(old, "  tls:\n") > strings.Index(old, "#  tls:") {
		t.Errorf("the section must be created just above the shipped example:\n%s", old)
	}

	// PSMDB 1.23.0's real cr.yaml, with the mode the section is created with.
	raw, err := os.ReadFile("testdata/cr-psmdb-1.23.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	psmdb := crSpecTLS(t, crTLSSection(string(raw), "dbcanvas-openbao", nil, "mode: preferTLS"))
	check("psmdb", psmdb)
	if psmdb["mode"] != "preferTLS" {
		t.Errorf("psmdb tls.mode = %v, want preferTLS", psmdb["mode"])
	}

	// Already pointed at an issuer (crTransform's replication path): left alone, not doubled.
	once := crTLSSection(crPXCLiveTLS, "dbcanvas-intranet-ca", nil)
	if twice := crTLSSection(once, "dbcanvas-openbao", nil); twice != once {
		t.Errorf("a second issuerConf was inserted:\n%s", twice)
	}
	// No issuer: untouched.
	if crTLSSection(crPXCLiveTLS, "", nil) != crPXCLiveTLS {
		t.Error("an empty issuer must leave cr.yaml alone")
	}
}

func TestK3DCRIssuerNeedsTheField(t *testing.T) {
	var said string
	cfg := &k3dConfig{OperatorVer: "0.1.0", CertIssuer: "something"}
	src := "spec:\n  mysql:\n    size: 3"
	if got := k3dCRIssuer(src, src, "dbcanvas-openbao", nil, cfg, func(s string) { said = s }); got != src {
		t.Errorf("a release without issuerConf must not get one:\n%s", got)
	}
	if !strings.Contains(said, "NOT used") || cfg.CertIssuer != "" {
		t.Errorf("the log must say so and the panel must not claim an issuer: said %q, cfg %q", said, cfg.CertIssuer)
	}
}

func TestK3DIssuerOf(t *testing.T) {
	f := designFrame{K3DOperator: "ps", K3DCertManager: true, K3DCertIssuer: "openbao"}
	if got := k3dIssuerOf(f); got != k3dIssuerOpenBao {
		t.Errorf("ps + cert-manager + openbao = %q", got)
	}
	noCM := f
	noCM.K3DCertManager = false
	pg := f
	pg.K3DOperator = "pg"
	junk := f
	junk.K3DCertIssuer = "letsencrypt"
	for name, fr := range map[string]designFrame{"no cert-manager": noCM, "pg operator": pg, "unknown issuer": junk} {
		if got := k3dIssuerOf(fr); got != "" {
			t.Errorf("%s: k3dIssuerOf = %q, want none", name, got)
		}
	}
}

func TestK3DIssuerIssues(t *testing.T) {
	cat := OperatorCatalog{
		"psmdb": {Latest: "1.23.1", Versions: []string{"1.23.1", "1.23.0", "1.22.0"}},
		"pxc":   {Latest: "1.20.0", Versions: []string{"1.20.0", "1.7.0"}},
	}
	bao := designDoc{Nodes: []designNode{{ID: "b1", Type: "openbao"}}}
	base := designFrame{Label: "k3d-00", K3DOperator: "pxc", K3DOperatorVer: "1.20.0", K3DCertManager: true}

	errs := func(iss []issue) (n int) {
		for _, i := range iss {
			if i.Level == "error" {
				n++
			}
		}
		return n
	}
	cases := []struct {
		name   string
		f      designFrame
		doc    designDoc
		errors int
		any    bool
	}{
		{"no issuer", base, designDoc{}, 0, false},
		{"intranet on pxc", with(base, func(f *designFrame) { f.K3DCertIssuer = "intranet" }), designDoc{}, 0, false},
		{"openbao with a node", with(base, func(f *designFrame) { f.K3DCertIssuer = "openbao" }), bao, 0, false},
		{"openbao without a node", with(base, func(f *designFrame) { f.K3DCertIssuer = "openbao" }), designDoc{}, 1, true},
		{"no cert-manager", with(base, func(f *designFrame) { f.K3DCertIssuer = "intranet"; f.K3DCertManager = false }), designDoc{}, 0, true},
		{"cnpg", with(base, func(f *designFrame) { f.K3DCertIssuer = "intranet"; f.K3DOperator = "cnpg" }), designDoc{}, 0, true},
		{"psmdb 1.23.1", with(base, func(f *designFrame) {
			f.K3DCertIssuer = "openbao"
			f.K3DOperator = "psmdb"
			f.K3DOperatorVer = "1.23.1"
		}), bao, 0, false},
		{"psmdb latest", with(base, func(f *designFrame) { f.K3DCertIssuer = "openbao"; f.K3DOperator = "psmdb"; f.K3DOperatorVer = "" }), bao, 0, false},
		{"psmdb 1.22.0", with(base, func(f *designFrame) {
			f.K3DCertIssuer = "openbao"
			f.K3DOperator = "psmdb"
			f.K3DOperatorVer = "1.22.0"
		}), bao, 1, true},
	}
	for _, c := range cases {
		iss := k3dIssuerIssues(c.f, c.doc, cat, false)
		if errs(iss) != c.errors || (len(iss) > 0) != c.any {
			t.Errorf("%s: got %v, want %d error(s), any=%v", c.name, iss, c.errors, c.any)
		}
	}
	// Running: the same problem is a warning, since nothing is re-applied.
	f := with(base, func(f *designFrame) { f.K3DCertIssuer = "openbao" })
	if iss := k3dIssuerIssues(f, designDoc{}, cat, true); len(iss) != 1 || iss[0].Level != "warning" {
		t.Errorf("a deployed frame must only warn, got %v", iss)
	}
}

func with(f designFrame, mod func(*designFrame)) designFrame {
	mod(&f)
	return f
}

func TestK3DOpenBaoIssuerManifest(t *testing.T) {
	m := k3dOpenBaoIssuerManifest("https://openbao.example.net:8200", "pki/sign/dbcanvas", "Q0E=", "role-123", "secret-456")
	docs := strings.Split(m, "---\n")
	if len(docs) != 2 {
		t.Fatalf("want a Secret and an Issuer, got %d documents:\n%s", len(docs), m)
	}
	var secret, iss map[string]any
	if err := yaml.Unmarshal([]byte(docs[0]), &secret); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(docs[1]), &iss); err != nil {
		t.Fatal(err)
	}
	if secret["stringData"].(map[string]any)["secretId"] != "secret-456" {
		t.Errorf("secret = %v", secret)
	}
	v := iss["spec"].(map[string]any)["vault"].(map[string]any)
	ar := v["auth"].(map[string]any)["appRole"].(map[string]any)
	ref := ar["secretRef"].(map[string]any)
	if v["server"] != "https://openbao.example.net:8200" || v["path"] != "pki/sign/dbcanvas" || v["caBundle"] != "Q0E=" ||
		ar["path"] != "approle" || ar["roleId"] != "role-123" || ref["name"] != k3dOpenBaoAppRoleSecret || ref["key"] != "secretId" {
		t.Errorf("issuer = %v", iss)
	}
	if strings.Contains(k3dOpenBaoIssuerManifest("http://openbao.example.net:8200", "p", "", "r", "s"), "caBundle") {
		t.Error("plain HTTP has no CA to bundle")
	}
}

func TestOpenBaoPKIPolicy(t *testing.T) {
	p := openbaoPKIPolicyHCL()
	if !strings.Contains(p, `path "pki/sign/dbcanvas"`) || !strings.Contains(p, `path "pki/sign/psmdb"`) || strings.Contains(p, "/issue/") || strings.Contains(p, "sudo") {
		t.Errorf("the policy must allow signing through the role and nothing broader:\n%s", p)
	}
}

// MongoDB signs through the role that gives its certificates the O=PSMDB its members match on.
func TestOpenBaoPKISignPath(t *testing.T) {
	for op, want := range map[string]string{"psmdb": "pki/sign/psmdb", "pxc": "pki/sign/dbcanvas", "ps": "pki/sign/dbcanvas"} {
		if got := openbaoPKISignPath(op); got != want {
			t.Errorf("%s: %s, want %s", op, got, want)
		}
	}
}
