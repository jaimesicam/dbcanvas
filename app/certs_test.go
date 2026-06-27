package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// makeTestCA builds a self-signed RSA CA (cert + PKCS#8 key PEM), mirroring the
// Intranet CA (`openssl req -x509 -newkey rsa:2048`).
func makeTestCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"DBCanvas"}, CommonName: "DBCanvas CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	return certPEM, keyPEM
}

func TestSignTLSCert_CASigned(t *testing.T) {
	caCert, caKey := makeTestCA(t)
	certPEM, keyPEM, err := signTLSCert(caCert, caKey, "seaweedfs-01.example.net",
		[]string{"seaweedfs-01.example.net", "seaweedfs-01"}, 48*time.Hour)
	if err != nil {
		t.Fatalf("signTLSCert: %v", err)
	}
	// The cert must parse, carry the SANs, and verify against the CA.
	leaf := parseLeaf(t, certPEM)
	if leaf.Subject.CommonName != "seaweedfs-01.example.net" {
		t.Errorf("CN = %q", leaf.Subject.CommonName)
	}
	if len(leaf.DNSNames) != 2 || leaf.DNSNames[0] != "seaweedfs-01.example.net" {
		t.Errorf("DNSNames = %v", leaf.DNSNames)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caCert) {
		t.Fatal("append CA")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "seaweedfs-01.example.net"}); err != nil {
		t.Errorf("verify against CA: %v", err)
	}
	// The key must pair with the cert (tls-loadable).
	if _, err := tlsKeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("cert/key pair: %v", err)
	}
}

func TestSignTLSCert_SelfSigned(t *testing.T) {
	certPEM, keyPEM, err := signTLSCert(nil, nil, "seaweedfs-01.example.net",
		[]string{"seaweedfs-01.example.net"}, 0)
	if err != nil {
		t.Fatalf("signTLSCert: %v", err)
	}
	leaf := parseLeaf(t, certPEM)
	// Self-signed: issuer == subject, ~365-day default TTL.
	if leaf.Issuer.CommonName != leaf.Subject.CommonName {
		t.Errorf("self-signed issuer %q != subject %q", leaf.Issuer.CommonName, leaf.Subject.CommonName)
	}
	if d := time.Until(leaf.NotAfter); d < 360*24*time.Hour {
		t.Errorf("default TTL too short: %v", d)
	}
	if _, err := tlsKeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("cert/key pair: %v", err)
	}
}

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	if b == nil {
		t.Fatal("cert not PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tlsKeyPair(certPEM, keyPEM []byte) (any, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	leaf, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	// Confirm the public keys match.
	if key.PublicKey.N.Cmp(leaf.PublicKey.(*rsa.PublicKey).N) != 0 {
		return nil, errMismatch
	}
	return leaf, nil
}

var errMismatch = &mismatchErr{}

type mismatchErr struct{}

func (*mismatchErr) Error() string { return "cert/key public keys differ" }
