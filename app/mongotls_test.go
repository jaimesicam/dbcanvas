package main

import (
	"strings"
	"testing"
)

// A certificate puts a net.tls block (preferTLS, the per-node cert, the Intranet CA) inside
// the one net: key of mongod.conf and mongos.conf — and nothing when it is off.
func TestMongoTLSConf(t *testing.T) {
	for name, conf := range map[string]string{
		"mongod": mongodConfYAML("rs0", "", true, "", "", true),
		"mongos": mongosConfYAML("cfg/h1:27017", "", true),
	} {
		if strings.Count(conf, "net:") != 1 {
			t.Errorf("%s: want exactly one net: key\n%s", name, conf)
		}
		want := "net:\n  port: 27017\n  bindIpAll: true\n  tls:\n    mode: preferTLS\n" +
			"    certificateKeyFile: /etc/mongo/certs/server.pem\n    CAFile: /etc/mongo/certs/ca.crt\n" +
			"    allowConnectionsWithoutCertificates: true\n"
		if !strings.Contains(conf, want) {
			t.Errorf("%s: tls block missing or misplaced\n%s", name, conf)
		}
	}
	if strings.Contains(mongodConfYAML("rs0", "", true, "", "", false), "tls:") ||
		strings.Contains(mongosConfYAML("cfg/h1:27017", "", false), "tls:") {
		t.Error("tls block written with TLS off")
	}
}

// A certificate turns TLS on, as it does for every other engine; no certificate, no TLS.
func TestMongoTLSOnFollowsCert(t *testing.T) {
	if mongoTLSOn(designFrame{}) {
		t.Error("TLS on without a certificate")
	}
	if !mongoTLSOn(designFrame{GenerateCert: true}) {
		t.Error("TLS off with a certificate")
	}
}

// cert=true on a MongoDB kind is what turns TLS on.
func TestComposeMongoCertTurnsTLSOn(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "psm", Cert: true},
		{Kind: "psmrs", Cert: true},
	}})
	psm := nodesOfType(doc, "psm")
	if len(psm) != 1 || !mongoTLSOn(designFrame{GenerateCert: psm[0].GenerateCert}) {
		t.Errorf("psm: %+v", psm)
	}
	for _, f := range doc.Frames {
		if f.Type == "psmrs" && !mongoTLSOn(f) {
			t.Errorf("psmrs frame: GenerateCert=%v", f.GenerateCert)
		}
	}
}

func TestSampleCodeMongoTLSVerify(t *testing.T) {
	if got := scDeriveProbeTLS(scMongoDB, scCfgProbe{GenerateCert: true, TLS: true}, "oraclelinux"); got.Mode != scTLSVerify {
		t.Errorf("TLS-on MongoDB derived %q, want verify", got.Mode)
	}
	if got := scDeriveProbeTLS(scMongoDB, scCfgProbe{GenerateCert: true}, "oraclelinux"); got.Mode != scTLSOff {
		t.Errorf("cert-only MongoDB derived %q, want off", got.Mode)
	}
}
