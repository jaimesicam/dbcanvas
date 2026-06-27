package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// signTLSCert returns a PEM server certificate + RSA private key for the given
// common name and DNS SANs, valid for ttl. When caCertPEM/caKeyPEM are both
// non-empty the certificate is signed by that CA (the Intranet CA, so clients that
// trust it verify the server); otherwise it is self-signed.
//
// This is the Go equivalent of the in-container `openssl` signing used by the
// systemd-image nodes (PXC/Patroni), for images that ship no openssl — e.g. the
// minimal SeaweedFS image.
func signTLSCert(caCertPEM, caKeyPEM []byte, cn string, dnsNames []string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	if ttl <= 0 {
		ttl = 365 * 24 * time.Hour
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"DBCanvas"}, CommonName: cn},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		BasicConstraintsValid: true,
	}

	parent := tmpl                        // self-signed by default
	var signerKey crypto.PrivateKey = key // self-signed: signed with its own key
	if len(caCertPEM) > 0 && len(caKeyPEM) > 0 {
		caCert, caKey, perr := parseCA(caCertPEM, caKeyPEM)
		if perr != nil {
			return nil, nil, perr
		}
		parent, signerKey = caCert, caKey
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// parseCA parses a PEM CA certificate + private key (the Intranet CA, created by
// `openssl req -x509 -newkey rsa:2048`). The key may be PKCS#8 ("BEGIN PRIVATE
// KEY", openssl's default) or PKCS#1 ("BEGIN RSA PRIVATE KEY").
func parseCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, crypto.PrivateKey, error) {
	cb, _ := pem.Decode(caCertPEM)
	if cb == nil {
		return nil, nil, fmt.Errorf("CA certificate is not valid PEM")
	}
	caCert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	kb, _ := pem.Decode(caKeyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("CA key is not valid PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(kb.Bytes); err == nil {
		return caCert, k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(kb.Bytes); err == nil {
		return caCert, k, nil
	}
	return nil, nil, fmt.Errorf("parse CA key: unsupported format")
}
