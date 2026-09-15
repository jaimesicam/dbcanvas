package main

// Database Explorer — TLS, and what "verify" can honestly mean from inside the app.
//
// DBCanvas reaches a node by its address on the stack's Docker network, because
// Docker's embedded DNS does not resolve the Intranet's *.<domain> names — every
// network-dialling tool here does the same (see dialNodeDSNPort). That has a
// consequence for TLS that is worth stating rather than papering over: the app
// connects to an IP, and a certificate issued for `pg-01.example.net` does not match
// an IP, so full verification would fail on the hostname for a certificate that is
// perfectly valid.
//
// So the posture is chain verification against the stack's own CA, without the
// hostname check. That is `verify-ca` in PostgreSQL's own vocabulary, and it is a
// real check: a certificate not issued by this stack's CA is rejected. What it is
// not is `InsecureSkipVerify` — nothing here turns verification off, globally or
// otherwise, and a node deployed without a certificate simply negotiates plaintext
// or opportunistic TLS rather than pretending to a guarantee it cannot make.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// dexCACache holds one stack's CA, read once off its Intranet. The read is a
// container exec; doing it per query would be absurd, and the CA does not change
// within a deployment.
var dexCACache = struct {
	sync.Mutex
	pem  map[int64][]byte
	file map[int64]string
}{pem: map[int64][]byte{}, file: map[int64]string{}}

// dexStackCA returns the stack CA in PEM form, or nil when the stack has no running
// Intranet (which is also the answer for a stack whose nodes were deployed without
// certificates — there is nothing to verify against).
func (a *App) dexStackCA(ctx context.Context, st Stack) []byte {
	dexCACache.Lock()
	if pem, ok := dexCACache.pem[st.ID]; ok {
		dexCACache.Unlock()
		return pem
	}
	dexCACache.Unlock()

	var pem []byte
	if id := a.intranetContainerID(ctx, st); id != "" {
		if b, err := a.readIntranetFile(ctx, id, "/etc/pki/dbcanvas/ca.crt"); err == nil {
			pem = b
		}
	}
	dexCACache.Lock()
	dexCACache.pem[st.ID] = pem
	dexCACache.Unlock()
	return pem
}

// dexStackCAFile writes the stack CA somewhere libpq can read it, because a pgx DSN
// names a CA by path and not by content. Written once per stack, into the app's own
// temp directory, and never containing anything secret — a CA certificate is public
// by construction.
func (a *App) dexStackCAFile(ctx context.Context, st Stack) string {
	dexCACache.Lock()
	if p, ok := dexCACache.file[st.ID]; ok {
		dexCACache.Unlock()
		return p
	}
	dexCACache.Unlock()

	path := ""
	if pem := a.dexStackCA(ctx, st); len(pem) > 0 {
		p := filepath.Join(os.TempDir(), fmt.Sprintf("dbcanvas-dex-ca-%d.crt", st.ID))
		if err := os.WriteFile(p, pem, 0o644); err == nil {
			path = p
		}
	}
	dexCACache.Lock()
	dexCACache.file[st.ID] = path
	dexCACache.Unlock()
	return path
}

// dexChainOnlyTLS builds a TLS config that verifies the peer's chain against the
// stack CA but does not check the name, for the drivers that take a *tls.Config
// rather than a DSN keyword (MySQL).
//
// InsecureSkipVerify is set only to take the *hostname* check out of the handshake's
// hands — the chain is then verified explicitly in VerifyPeerCertificate against the
// CA pool, and a certificate that does not chain to this stack's CA closes the
// connection. Dropping the callback would be the insecure version of this; it is not
// what is happening here.
func dexChainOnlyTLS(caPEM []byte) *tls.Config {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("the server presented no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			inter := x509.NewCertPool()
			for _, raw := range rawCerts[1:] {
				if c, err := x509.ParseCertificate(raw); err == nil {
					inter.AddCert(c)
				}
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter})
			if err != nil {
				return fmt.Errorf("the server's certificate was not issued by this stack's CA: %v", err)
			}
			return nil
		},
	}
}
