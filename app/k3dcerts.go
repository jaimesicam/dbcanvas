package main

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// k3dcerts.go — the Certificates tab: what the cluster's TLS Secrets actually hold, and whether it
// is valid.
//
// Read live on every request, never recorded at deploy: cert-manager renews a certificate by
// rewriting its Secret in place, and the tab is only worth having if a renewal shows up in it. The
// panel asks again on a timer, and every certificate carries its serial and fingerprint, so a
// renewal is visible as a changed serial and a new not-before.
//
// Every Secret in the cluster's namespace that carries a tls.crt is shown — the operator's
// <cluster>-ssl / -ssl-internal, the CA a CA Issuer signs from, anything a user added — with the
// cert-manager Certificate that owns it, when one does (ready, renewal time, revision, issuer).
//
// Each one is checked three ways, because they fail for different reasons:
//
//	chain    tls.crt verifies against ca.crt, using any further certificates in tls.crt as
//	         intermediates — what Go, a browser or mongosh does.
//	strict   the same with NO intermediates: `openssl verify -CAfile ca.crt tls.crt`, which is what
//	         Percona XtraDB Cluster's SST script runs on every joiner (openbaopki.go). A chain that
//	         passes the first and fails this one is a cluster whose SST will fail.
//	key      tls.key is the private half of the certificate's public key.
//
// plus the plain dates: not yet valid, expired, or expiring within k3dCertSoon.

// k3dCertSoon is how close to expiry a certificate is flagged.
const k3dCertSoon = 14 * 24 * time.Hour

// k3dCertInfo is one parsed certificate.
type k3dCertInfo struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	DaysLeft    int       `json:"daysLeft"`
	DNSNames    []string  `json:"dnsNames,omitempty"`
	IPs         []string  `json:"ips,omitempty"`
	Usages      []string  `json:"usages,omitempty"`
	IsCA        bool      `json:"isCA"`
	SelfSigned  bool      `json:"selfSigned"`
	Fingerprint string    `json:"fingerprint"` // SHA-256 of the DER, hex
}

// k3dCertCheck is one validity check.
type k3dCertCheck struct {
	Name   string `json:"name"`   // chain | strict | key | dates
	OK     bool   `json:"ok"`     //
	Detail string `json:"detail"` // what passed, or why it failed
}

// k3dCertManaged is the cert-manager Certificate that owns a Secret.
type k3dCertManaged struct {
	Name        string `json:"name"`
	Ready       bool   `json:"ready"`
	Reason      string `json:"reason,omitempty"` // the Ready condition's message when it is not
	Issuer      string `json:"issuer"`           // kind/name
	RenewalTime string `json:"renewalTime,omitempty"`
	Revision    int    `json:"revision,omitempty"`
}

// k3dCertSecret is one Secret as the tab shows it.
type k3dCertSecret struct {
	Secret     string          `json:"secret"`
	Version    string          `json:"resourceVersion"`
	Status     string          `json:"status"` // valid | warning | invalid
	Chain      []k3dCertInfo   `json:"chain"`  // tls.crt, leaf first
	CA         []k3dCertInfo   `json:"ca"`     // ca.crt
	Checks     []k3dCertCheck  `json:"checks"`
	CertMgr    *k3dCertManaged `json:"certManager,omitempty"`
	ParseError string          `json:"parseError,omitempty"`
}

// k3dCertsView is the tab.
type k3dCertsView struct {
	Namespace string          `json:"namespace"`
	CheckedAt time.Time       `json:"checkedAt"`
	Secrets   []k3dCertSecret `json:"secrets"`
}

func (a *App) handleK3DCerts(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	ns := orDefault(cfg.Namespace, "default")
	view, err := a.k3dCertsRead(r.Context(), dep.ContainerID, ns, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *App) k3dCertsRead(ctx context.Context, serverID, ns string, now time.Time) (k3dCertsView, error) {
	secrets, err := a.kubectl(ctx, serverID, "-n", ns, "get", "secrets", "-o", "json")
	if err != nil {
		return k3dCertsView{}, fmt.Errorf("read the Secrets: %w", err)
	}
	// No cert-manager is not an error: the operator then made its own Secrets, which are still shown.
	certs, _ := a.kubectl(ctx, serverID, "-n", ns, "get", "certificates.cert-manager.io", "-o", "json")
	return k3dCertsFrom(ns, secrets, certs, now), nil
}

// k3dCertsFrom builds the view from `kubectl get secrets -o json` and `kubectl get certificates -o
// json` ("" when cert-manager is not installed).
func k3dCertsFrom(ns, secretsJSON, certsJSON string, now time.Time) k3dCertsView {
	var secrets struct {
		Items []struct {
			Metadata struct {
				Name            string `json:"name"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	json.Unmarshal([]byte(secretsJSON), &secrets)
	owners := k3dCertManagersBySecret(certsJSON)

	view := k3dCertsView{Namespace: ns, CheckedAt: now.UTC(), Secrets: []k3dCertSecret{}}
	for _, s := range secrets.Items {
		raw, ok := s.Data["tls.crt"]
		if !ok {
			continue
		}
		dec := func(k string) []byte { b, _ := base64.StdEncoding.DecodeString(s.Data[k]); return b }
		cs := k3dCheckSecret(dec("tls.crt"), dec("ca.crt"), dec("tls.key"), now)
		if raw == "" {
			cs.ParseError = "tls.crt is empty"
			cs.Status = "invalid"
		}
		cs.Secret, cs.Version = s.Metadata.Name, s.Metadata.ResourceVersion
		cs.CertMgr = owners[s.Metadata.Name]
		if cs.CertMgr != nil && !cs.CertMgr.Ready && cs.Status == "valid" {
			cs.Status = "warning"
		}
		view.Secrets = append(view.Secrets, cs)
	}
	sort.Slice(view.Secrets, func(i, j int) bool { return view.Secrets[i].Secret < view.Secrets[j].Secret })
	return view
}

// k3dCertManagersBySecret maps each Secret name to the cert-manager Certificate that writes it.
func k3dCertManagersBySecret(js string) map[string]*k3dCertManaged {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				SecretName string `json:"secretName"`
				IssuerRef  struct {
					Name string `json:"name"`
					Kind string `json:"kind"`
				} `json:"issuerRef"`
			} `json:"spec"`
			Status struct {
				RenewalTime string `json:"renewalTime"`
				Revision    int    `json:"revision"`
				Conditions  []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	out := map[string]*k3dCertManaged{}
	if json.Unmarshal([]byte(js), &list) != nil {
		return out
	}
	for _, c := range list.Items {
		m := &k3dCertManaged{Name: c.Metadata.Name, Issuer: orDefault(c.Spec.IssuerRef.Kind, "Issuer") + "/" + c.Spec.IssuerRef.Name,
			RenewalTime: c.Status.RenewalTime, Revision: c.Status.Revision}
		for _, cond := range c.Status.Conditions {
			if cond.Type == "Ready" {
				m.Ready = cond.Status == "True"
				if !m.Ready {
					m.Reason = cond.Message
				}
			}
		}
		out[c.Spec.SecretName] = m
	}
	return out
}

// k3dParsePEMCerts parses every CERTIFICATE block, in order.
func k3dParsePEMCerts(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

var k3dEKUNames = map[x509.ExtKeyUsage]string{
	x509.ExtKeyUsageServerAuth: "server auth", x509.ExtKeyUsageClientAuth: "client auth",
	x509.ExtKeyUsageCodeSigning: "code signing", x509.ExtKeyUsageEmailProtection: "email",
	x509.ExtKeyUsageOCSPSigning: "OCSP signing", x509.ExtKeyUsageAny: "any",
}

func k3dCertInfoOf(c *x509.Certificate, now time.Time) k3dCertInfo {
	sum := sha256.Sum256(c.Raw)
	info := k3dCertInfo{
		Subject: c.Subject.String(), Issuer: c.Issuer.String(), Serial: strings.ToUpper(c.SerialNumber.Text(16)),
		NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC(), DaysLeft: int(c.NotAfter.Sub(now).Hours() / 24),
		DNSNames: c.DNSNames, IsCA: c.IsCA, Fingerprint: hex.EncodeToString(sum[:]),
		SelfSigned: c.CheckSignatureFrom(c) == nil && c.Subject.String() == c.Issuer.String(),
	}
	for _, ip := range c.IPAddresses {
		info.IPs = append(info.IPs, ip.String())
	}
	for _, u := range c.ExtKeyUsage {
		info.Usages = append(info.Usages, orDefault(k3dEKUNames[u], fmt.Sprint(u)))
	}
	return info
}

// k3dCheckSecret parses and checks one Secret's tls.crt / ca.crt / tls.key.
func k3dCheckSecret(tlsCrt, caCrt, tlsKey []byte, now time.Time) k3dCertSecret {
	var cs k3dCertSecret
	chain, err := k3dParsePEMCerts(tlsCrt)
	if err != nil || len(chain) == 0 {
		cs.ParseError = "tls.crt holds no certificate that parses"
		if err != nil {
			cs.ParseError = "tls.crt: " + err.Error()
		}
		cs.Status = "invalid"
		return cs
	}
	cas, caErr := k3dParsePEMCerts(caCrt)
	for _, c := range chain {
		cs.Chain = append(cs.Chain, k3dCertInfoOf(c, now))
	}
	for _, c := range cas {
		cs.CA = append(cs.CA, k3dCertInfoOf(c, now))
	}
	leaf := chain[0]
	add := func(name string, ok bool, detail string) {
		cs.Checks = append(cs.Checks, k3dCertCheck{Name: name, OK: ok, Detail: detail})
	}

	// Dates.
	switch {
	case now.Before(leaf.NotBefore):
		add("dates", false, "not valid until "+leaf.NotBefore.UTC().Format(time.RFC3339))
	case now.After(leaf.NotAfter):
		add("dates", false, "expired "+leaf.NotAfter.UTC().Format(time.RFC3339))
	default:
		add("dates", true, fmt.Sprintf("valid until %s (%d days)", leaf.NotAfter.UTC().Format(time.RFC3339), int(leaf.NotAfter.Sub(now).Hours()/24)))
	}

	// Chain and strict, against ca.crt. A Secret with no ca.crt (a CA's own Secret, or one made by
	// hand) is checked against itself when self-signed, and otherwise said to have nothing to check.
	switch {
	case caErr != nil:
		add("chain", false, "ca.crt: "+caErr.Error())
	case len(cas) == 0 && !cs.Chain[0].SelfSigned:
		add("chain", false, "no ca.crt to verify against")
	default:
		roots := x509.NewCertPool()
		for _, c := range cas {
			roots.AddCert(c)
		}
		if len(cas) == 0 {
			roots.AddCert(leaf)
		}
		inter := x509.NewCertPool()
		for _, c := range chain[1:] {
			inter.AddCert(c)
		}
		opts := x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
		if _, err := leaf.Verify(opts); err != nil {
			add("chain", false, err.Error())
		} else {
			add("chain", true, "tls.crt chains to ca.crt")
		}
		if len(chain) > 1 {
			opts.Intermediates = x509.NewCertPool()
			if _, err := leaf.Verify(opts); err != nil {
				add("strict", false, "fails `openssl verify -CAfile ca.crt tls.crt` (no intermediates), which PXC's SST runs: "+err.Error())
			} else {
				add("strict", true, "verifies against ca.crt alone, as `openssl verify -CAfile` does")
			}
		}
	}

	// Key.
	if len(tlsKey) > 0 {
		if ok, why := k3dKeyMatches(leaf, tlsKey); ok {
			add("key", true, "tls.key matches the certificate")
		} else {
			add("key", false, why)
		}
	}

	cs.Status = "valid"
	for _, c := range cs.Checks {
		if !c.OK {
			cs.Status = "invalid"
		}
	}
	if cs.Status == "valid" && leaf.NotAfter.Sub(now) < k3dCertSoon {
		cs.Status = "warning"
	}
	return cs
}

// k3dKeyMatches reports whether a PEM private key is the certificate's.
func k3dKeyMatches(c *x509.Certificate, keyPEM []byte) (bool, string) {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return false, "tls.key is not valid PEM"
	}
	var key any
	var err error
	if key, err = x509.ParsePKCS8PrivateKey(blk.Bytes); err != nil {
		if key, err = x509.ParsePKCS1PrivateKey(blk.Bytes); err != nil {
			if key, err = x509.ParseECPrivateKey(blk.Bytes); err != nil {
				return false, "tls.key: unsupported key format"
			}
		}
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return false, "tls.key is not a signing key"
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(c.PublicKey) {
		return false, "tls.key does NOT match the certificate's public key"
	}
	return true, ""
}
