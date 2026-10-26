package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// k3dReplSSLDoc is k3dReplDoc with SSL chosen on the replica's frame.
func k3dReplSSLDoc(skipVerify bool) designDoc {
	doc := k3dReplDoc("async")
	doc.Frames[1].K3DReplSSL = true
	doc.Frames[1].K3DReplSSLSkipVerify = skipVerify
	return doc
}

// The choice is made on the replica but decides both clusters' certificates: the replica's need the
// shared CA to verify with, and the source's are what get verified — so only the source's name the
// addresses, and only when the replica checks them.
func TestK3DReplTLSFor(t *testing.T) {
	doc := k3dReplSSLDoc(false)
	if got := k3dReplTLSFor(doc, doc.Frames[0]); !got.On || !got.SANs {
		t.Errorf("source of a verifying link = %+v, want certificates with SANs", got)
	}
	if got := k3dReplTLSFor(doc, doc.Frames[1]); !got.On || got.SANs {
		t.Errorf("replica = %+v, want certificates from the shared CA and no SANs", got)
	}
	doc = k3dReplSSLDoc(true)
	if got := k3dReplTLSFor(doc, doc.Frames[0]); !got.On || got.SANs {
		t.Errorf("source of a link with sslSkipVerify = %+v, want the shared CA and no SANs", got)
	}
	plain := k3dReplDoc("async")
	for _, f := range plain.Frames {
		if got := k3dReplTLSFor(plain, f); got.On {
			t.Errorf("%s on a plaintext link = %+v, want the operator's own certificates", f.Label, got)
		}
	}
	// SSL on a frame that is not the replica of anything changes nothing: there is no channel.
	plain.Frames[0].K3DReplSSL = true
	if got := k3dReplTLSFor(plain, plain.Frames[0]); got.On {
		t.Errorf("SSL on a source's own frame = %+v, want nothing — the option belongs to the replica", got)
	}
}

// The channel's configuration. Retry settings are stated because the operator only defaults them
// when `configuration` is absent, and the CA is the replica's own mounted ca.crt.
func TestK3DReplChannelConfig(t *testing.T) {
	links := k3dReplLinks(k3dReplDoc("async"))
	if c := k3dReplChannelConfig(links[0]); c != nil {
		t.Errorf("a plaintext link carries no configuration, got %v", c)
	}
	for _, skip := range []bool{false, true} {
		l := k3dReplLinks(k3dReplSSLDoc(skip))[0]
		c := k3dReplChannelConfig(l)
		if c["ssl"] != true || c["sslSkipVerify"] != skip || c["ca"] != "/etc/mysql/ssl/ca.crt" {
			t.Errorf("skipVerify=%v: configuration = %v", skip, c)
		}
		if c["sourceRetryCount"] != 3 || c["sourceConnectRetry"] != 60 {
			t.Errorf("the operator's retry defaults must be stated: %v", c)
		}
		if k3dReplVerifies(l) == skip {
			t.Errorf("k3dReplVerifies with sslSkipVerify=%v", skip)
		}
	}
}

// Both certificates carry the names the operator would have given them, and — with verification —
// every address of the MetalLB block plus the pod DNS names.
func TestK3DReplCertSpecs(t *testing.T) {
	ext, in, extra := k3dReplCertSpecs("cluster1", "pxc", "example.net", "172.20.255.248-172.20.255.255", true)
	if ext.Secret != "cluster1-ssl" || in.Secret != "cluster1-ssl-internal" {
		t.Fatalf("secrets = %s, %s — the operator only adopts these names", ext.Secret, in.Secret)
	}
	wantExtra := []string{"cluster1-pxc-0.example.net", "cluster1-pxc-1.example.net", "cluster1-pxc-2.example.net"}
	if strings.Join(extra, ",") != strings.Join(wantExtra, ",") {
		t.Errorf("extra DNS = %v, want %v", extra, wantExtra)
	}
	for _, c := range []k3dReplCertSpec{ext, in} {
		if len(c.IPs) != 8 || c.IPs[0].String() != "172.20.255.248" || c.IPs[7].String() != "172.20.255.255" {
			t.Errorf("%s IPs = %v, want the whole block", c.Secret, c.IPs)
		}
		joined := strings.Join(c.DNS, ",")
		for _, want := range append([]string{"*.cluster1-pxc"}, wantExtra...) {
			if !strings.Contains(joined, want) {
				t.Errorf("%s does not name %s: %v", c.Secret, want, c.DNS)
			}
		}
	}
	// HAProxy passes TLS through to the pod, which serves the internal certificate — so it is
	// the internal one that has to name the HAProxy Services.
	if !strings.Contains(strings.Join(in.DNS, ","), "cluster1-haproxy.pxc.svc.cluster.local") {
		t.Errorf("the internal certificate must name the HAProxy Service: %v", in.DNS)
	}
	ext, in, extra = k3dReplCertSpecs("cluster1", "pxc", "example.net", "172.20.255.248-172.20.255.255", false)
	if len(extra) != 0 || len(ext.IPs) != 0 || len(in.IPs) != 0 {
		t.Errorf("without verification nothing extra is named: %v %v %v", extra, ext.IPs, in.IPs)
	}
}

// A certificate signed for a verifying link must verify against the CA for each name a replica can
// dial, and serve as a client certificate too (Galera presents it to its peers).
func TestK3DReplIssuedCertVerifies(t *testing.T) {
	caCrt, caKey := testCA(t)
	_, in, _ := k3dReplCertSpecs("cluster1", "pxc", "example.net", "172.20.255.248-172.20.255.255", true)
	crtPEM, _, err := issueTLSCert(caCrt, caKey, in.CN, in.DNS, in.IPs, time.Hour,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caCrt)
	b, _ := pem.Decode(crtPEM)
	crt, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"cluster1-pxc-0.example.net", "172.20.255.250", "cluster1-pxc-1.cluster1-pxc"} {
		for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
			if _, err := crt.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
				t.Errorf("%s (usage %v): %v", host, usage, err)
			}
		}
	}
	if _, err := crt.Verify(x509.VerifyOptions{DNSName: "172.20.255.247", Roots: roots}); err == nil {
		t.Error("an address outside the frame's block must not verify")
	}
}

// testCA makes a self-signed CA the way the Intranet's `openssl req -x509` does.
func testCA(t *testing.T) ([]byte, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "DBCanvas CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// The cert-manager Certificate carries the addresses as ipAddresses — the thing the operator's own
// tls.SANs cannot do — and is issued by the shared CA's Issuer.
func TestK3DReplCertificateManifest(t *testing.T) {
	_, in, _ := k3dReplCertSpecs("cluster1", "pxc", "example.net", "172.20.255.248-172.20.255.249", true)
	m := k3dReplCertificateManifest(in)
	for _, want := range []string{
		"name: cluster1-ssl-internal", "secretName: cluster1-ssl-internal",
		"ipAddresses:\n  - 172.20.255.248\n  - 172.20.255.249\n",
		`- "cluster1-pxc-0.example.net"`, `- "*.cluster1-pxc"`,
		"- client auth", "issuerRef:\n    name: dbcanvas-intranet-ca\n    kind: Issuer",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Certificate is missing %q:\n%s", want, m)
		}
	}
	if !strings.Contains(k3dReplIssuerManifest(), "ca:\n    secretName: dbcanvas-intranet-ca") {
		t.Error("the Issuer must be a CA Issuer over the Intranet CA secret")
	}
}

// The Secret the manual path applies is the shape the operator mounts.
func TestK3DReplTLSSecret(t *testing.T) {
	var s struct {
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(k3dReplTLSSecret("cluster1-ssl", []byte("ca"), []byte("crt"), []byte("key")), &s); err != nil {
		t.Fatal(err)
	}
	if s.Type != "kubernetes.io/tls" || s.Data["ca.crt"] == "" || s.Data["tls.crt"] == "" || s.Data["tls.key"] == "" {
		t.Errorf("secret = %+v", s)
	}
}

// cr.yaml's tls section: SANs and issuerConf inserted live beside `enabled`, the shipped examples
// left commented and marked.
func TestCRTLSIssuerAndSANs(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	out := crTransform(string(raw), crOptions{Name: "cluster1", Proxy: "haproxy",
		TLSIssuer: "dbcanvas-intranet-ca", TLSSANs: []string{"cluster1-pxc-0.example.net", "cluster1-pxc-1.example.net"}})
	sec := crSection(out, "tls")
	for _, want := range []string{
		"    SANs:\n    - cluster1-pxc-0.example.net\n    - cluster1-pxc-1.example.net\n",
		"    issuerConf:\n      name: dbcanvas-intranet-ca\n      kind: Issuer\n      group: cert-manager.io\n",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("tls section is missing %q:\n%s", want, sec)
		}
	}
	if countLive(sec, "SANs:") != 1 || countLive(sec, "issuerConf:") != 1 {
		t.Errorf("want one live SANs and issuerConf:\n%s", sec)
	}
	if plain := crTransform(string(raw), crOptions{Name: "cluster1", Proxy: "haproxy"}); countLive(crSection(plain, "tls"), "issuerConf:") != 0 {
		t.Error("without a replication link over SSL the tls section stays as shipped")
	}
}

// Addresses keyed by pod, so a verifying replica can dial each by its published name.
func TestK3DReplAddrsFromServices(t *testing.T) {
	js := `{"items":[
	  {"spec":{"type":"LoadBalancer","selector":{"app.kubernetes.io/component":"haproxy"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.246"}]}}},
	  {"spec":{"type":"LoadBalancer","selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-1"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.249"}]}}},
	  {"spec":{"type":"LoadBalancer","selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-0"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.248"}]}}}
	]}`
	got := k3dReplAddrsFromServices(js)
	if len(got) != 2 || got["cluster1-pxc-0"] != "172.20.255.248" || got["cluster1-pxc-1"] != "172.20.255.249" {
		t.Errorf("by pod = %v", got)
	}
}

// The published names come from the K3D members' recorded configs, once each.
func TestK3DReplDNSRecords(t *testing.T) {
	cfg, _ := json.Marshal(k3dConfig{ReplHosts: map[string]string{"cluster1-pxc-0": "172.20.255.248", "bad": "nope"}})
	deps := []Deployment{{NodeID: "n1", Config: cfg}, {NodeID: "n1b", Config: cfg}, {NodeID: "x", Config: cfg}}
	recs := k3dReplDNSRecords(deps, map[string]string{"n1": "k3d", "n1b": "k3d", "x": "pxc"})
	if len(recs) != 1 || recs[0].host != "cluster1-pxc-0" || recs[0].ip != "172.20.255.248" {
		t.Errorf("records = %+v", recs)
	}
}

// Verifying replicas dial names built from the source's cluster name, so two frames sharing one
// would publish the same names for different clusters.
func TestK3DReplValidationSSLNeedsUniqueClusterNames(t *testing.T) {
	doc := k3dReplSSLDoc(false)
	if issues := k3dReplValidate(doc); hasError(issues) {
		t.Fatalf("a well-formed SSL link must not error: %+v", issues)
	}
	other := k3dFrame("f3", "cluster1")
	doc.Frames = append(doc.Frames, other)
	if !hasIssue(k3dReplValidate(doc), "error", "same cluster name") {
		t.Error("a second frame with the source's cluster name must be refused")
	}
	doc.Frames[1].K3DReplSSLSkipVerify = true
	if hasIssue(k3dReplValidate(doc), "error", "same cluster name") {
		t.Error("without verification no names are published, so the clash does not matter")
	}
}

func TestK3DReplPoolIPs(t *testing.T) {
	if got := k3dReplPoolIPs("172.20.255.248-172.20.255.250"); len(got) != 3 || !got[2].Equal(net.ParseIP("172.20.255.250")) {
		t.Errorf("got %v", got)
	}
	if got := k3dReplPoolIPs(""); got != nil {
		t.Errorf("no range, got %v", got)
	}
}
