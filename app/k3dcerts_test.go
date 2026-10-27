package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// k3dTestCA makes a CA certificate + key, signed by parent (nil = self-signed).
func k3dTestCA(t *testing.T, cn string, parent *x509.Certificate, parentKey *rsa.PrivateKey) (*x509.Certificate, *rsa.PrivateKey, []byte) {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if parent == nil {
		parent, parentKey = tmpl, k
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &k.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, k, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func k3dTestLeaf(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, ttl time.Duration) ([]byte, []byte) {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "cluster1-pxc"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(ttl), DNSNames: []string{"cluster1-pxc", "*.cluster1-pxc"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func checkNamed(cs k3dCertSecret, name string) *k3dCertCheck {
	for i := range cs.Checks {
		if cs.Checks[i].Name == name {
			return &cs.Checks[i]
		}
	}
	return nil
}

func TestK3DCheckSecret(t *testing.T) {
	now := time.Now()
	root, rootKey, rootPEM := k3dTestCA(t, "DBCanvas CA", nil, nil)

	// Signed directly by the root: everything passes, and there is no strict check to make.
	crt, key := k3dTestLeaf(t, root, rootKey, 90*24*time.Hour)
	cs := k3dCheckSecret(crt, rootPEM, key, now)
	if cs.Status != "valid" || checkNamed(cs, "strict") != nil || !checkNamed(cs, "chain").OK || !checkNamed(cs, "key").OK {
		t.Errorf("direct: %+v", cs)
	}
	if cs.Chain[0].Subject != "CN=cluster1-pxc" || len(cs.Chain[0].DNSNames) != 2 || cs.CA[0].Subject != "CN=DBCanvas CA" || !cs.CA[0].SelfSigned {
		t.Errorf("details: %+v / %+v", cs.Chain[0], cs.CA[0])
	}

	// Through an intermediate carried in tls.crt: the chain verifies, the strict (openssl -CAfile)
	// check does not — the case that fails PXC's SST.
	inter, interKey, interPEM := k3dTestCA(t, "Intermediate", root, rootKey)
	leaf, key2 := k3dTestLeaf(t, inter, interKey, 90*24*time.Hour)
	cs = k3dCheckSecret(append(append(leaf, '\n'), interPEM...), rootPEM, key2, now)
	if !checkNamed(cs, "chain").OK || checkNamed(cs, "strict") == nil || checkNamed(cs, "strict").OK || cs.Status != "invalid" {
		t.Errorf("intermediate: %+v", cs.Checks)
	}
	if !strings.Contains(checkNamed(cs, "strict").Detail, "SST") {
		t.Errorf("the strict failure must say why it matters: %q", checkNamed(cs, "strict").Detail)
	}

	// The wrong key, a stranger CA, an expiring certificate.
	if c := checkNamed(k3dCheckSecret(crt, rootPEM, key2, now), "key"); c == nil || c.OK {
		t.Error("a key from another certificate must fail")
	}
	_, _, otherPEM := k3dTestCA(t, "Someone else", nil, nil)
	if c := checkNamed(k3dCheckSecret(crt, otherPEM, key, now), "chain"); c == nil || c.OK {
		t.Error("a certificate from another CA must fail the chain check")
	}
	soon, soonKey := k3dTestLeaf(t, root, rootKey, 3*24*time.Hour)
	if cs := k3dCheckSecret(soon, rootPEM, soonKey, now); cs.Status != "warning" {
		t.Errorf("three days left: status %s, want warning", cs.Status)
	}
	if cs := k3dCheckSecret(soon, rootPEM, soonKey, now.Add(4*24*time.Hour)); cs.Status != "invalid" || checkNamed(cs, "dates").OK {
		t.Errorf("expired: %+v", cs.Checks)
	}
	if cs := k3dCheckSecret([]byte("junk"), rootPEM, nil, now); cs.Status != "invalid" || cs.ParseError == "" {
		t.Errorf("junk: %+v", cs)
	}
}

func TestK3DCertsFrom(t *testing.T) {
	root, rootKey, rootPEM := k3dTestCA(t, "DBCanvas CA", nil, nil)
	crt, key := k3dTestLeaf(t, root, rootKey, 90*24*time.Hour)
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	secrets, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "cluster1-ssl-internal", "resourceVersion": "42"},
			"data": map[string]string{"tls.crt": b64(crt), "ca.crt": b64(rootPEM), "tls.key": b64(key)}},
		map[string]any{"metadata": map[string]any{"name": "cluster1-secrets"}, "data": map[string]string{"root": "eA=="}},
	}})
	certs, _ := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"name": "cluster1-ssl-internal"},
		"spec":     map[string]any{"secretName": "cluster1-ssl-internal", "issuerRef": map[string]any{"name": "dbcanvas-openbao", "kind": "Issuer"}},
		"status": map[string]any{"renewalTime": "2027-01-01T00:00:00Z", "revision": 2,
			"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "Issuing certificate as Secret does not exist"}}},
	}}})
	v := k3dCertsFrom("default", string(secrets), string(certs), time.Now())
	if len(v.Secrets) != 1 {
		t.Fatalf("only Secrets with a tls.crt are shown, got %d", len(v.Secrets))
	}
	s := v.Secrets[0]
	if s.Secret != "cluster1-ssl-internal" || s.Version != "42" || s.CertMgr == nil || s.CertMgr.Issuer != "Issuer/dbcanvas-openbao" ||
		s.CertMgr.Revision != 2 || s.CertMgr.Ready || s.CertMgr.Reason == "" {
		t.Errorf("secret: %+v / %+v", s, s.CertMgr)
	}
	if !strings.Contains(s.TLSPEM, "BEGIN CERTIFICATE") || !strings.Contains(s.CAPEM, "BEGIN CERTIFICATE") || strings.Contains(s.TLSPEM+s.CAPEM, "PRIVATE KEY") {
		t.Errorf("the view carries the public PEMs and never the key")
	}
	if s.Status != "warning" {
		t.Errorf("a valid certificate whose Certificate is not Ready is a warning, got %s", s.Status)
	}
	if v := k3dCertsFrom("default", string(secrets), "", time.Now()); v.Secrets[0].CertMgr != nil || v.Secrets[0].Status != "valid" {
		t.Errorf("without cert-manager: %+v", v.Secrets[0])
	}
}
