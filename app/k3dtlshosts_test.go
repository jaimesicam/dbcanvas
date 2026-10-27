package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestCRTLSSectionSANs(t *testing.T) {
	sans := k3dTLSHostSANs("pxc", "cluster1", "example.net", false)
	for _, want := range []string{"cluster1-haproxy.example.net", "cluster1-proxysql.example.net", "cluster1-pxc-2.example.net"} {
		if !contains(sans, want) {
			t.Errorf("PXC host names lack %s: %v", want, sans)
		}
	}
	// Live section (PXC 1.15.0+): SANs and the issuer go in beside `enabled`.
	live := crSpecTLS(t, crTLSSection(crPXCLiveTLS, "dbcanvas-openbao", sans))
	got, _ := live["SANs"].([]any)
	if len(got) != len(sans) || live["enabled"] != true || live["issuerConf"] == nil {
		t.Errorf("live tls = %v", live)
	}
	// Commented section (older PXC, every PS): one is created carrying both.
	old := crSpecTLS(t, crTLSSection(crPXCCommentedTLS, "", sans))
	if got, _ := old["SANs"].([]any); len(got) != len(sans) || old["issuerConf"] != nil {
		t.Errorf("created tls = %v", old)
	}
	// SANs already there (crTransform wrote them for a live section): not doubled.
	once := crTLSSection(crPXCLiveTLS, "", sans)
	if twice := crTLSSection(once, "", sans); twice != once {
		t.Errorf("SANs were inserted twice:\n%s", twice)
	}
}

func TestK3DCRIssuerSANsNeedTheField(t *testing.T) {
	cfg := &k3dConfig{OperatorVer: "0.1.0"}
	src := "spec:\n  mysql:\n    size: 3"
	if got := k3dCRIssuer(src, src, "", []string{"a.example.net"}, cfg, func(string) {}); got != src || cfg.TLSHostsInCert {
		t.Errorf("a release without tls.SANs must get none and say so: %q, inCert=%v", got, cfg.TLSHostsInCert)
	}
	withField := "spec:\n#  tls:\n#    SANs:\n#    - x\n  mysql:\n    size: 3"
	if got := k3dCRIssuer(withField, withField, "", []string{"a.example.net"}, cfg, func(string) {}); !strings.Contains(got, "    - a.example.net") || !cfg.TLSHostsInCert {
		t.Errorf("want the SAN and inCert=true, got inCert=%v\n%s", cfg.TLSHostsInCert, got)
	}
}

// MongoDB names its hosts with a split horizon per replica set pod, inserted beside `size:`.
func TestPSMDBSplitHorizons(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-psmdb-1.23.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := psmdbTransform(string(raw), psmdbOptions{Name: "mongo1", ExposeReplset: "LoadBalancer",
		Horizons: psmdbSplitHorizons("mongo1", "example.net")})
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not valid YAML: %v", err)
	}
	rs := doc["spec"].(map[string]any)["replsets"].([]any)[0].(map[string]any)
	h, _ := rs["splitHorizons"].(map[string]any)
	for i, pod := range []string{"mongo1-rs0-0", "mongo1-rs0-1", "mongo1-rs0-2"} {
		ext, _ := h[pod].(map[string]any)
		if ext["external"] != pod+".example.net" {
			t.Errorf("horizon %d = %v, want %s.example.net (all: %v)", i, ext, pod, h)
		}
	}
	if k3dTLSHostCandidates("psmdb", "mongo1", true) != nil {
		t.Error("a sharded cluster has no replica set horizons to name")
	}
}

func TestK3DLBAddrsFromServices(t *testing.T) {
	js, _ := json.Marshal(map[string]any{"items": []any{
		lbSvc("cluster1-haproxy", "LoadBalancer", "172.20.255.241"),
		lbSvc("cluster1-haproxy-replicas", "LoadBalancer", "172.20.255.242"),
		lbSvc("cluster1-pxc", "ClusterIP", ""),
		lbSvc("cluster1-proxysql", "LoadBalancer", ""), // pending: no address yet
		lbSvc("other-haproxy", "LoadBalancer", "172.20.255.243"),
	}})
	got := k3dLBAddrsFromServices(string(js), "cluster1")
	if len(got) != 2 || got["cluster1-haproxy"] != "172.20.255.241" || got["cluster1-haproxy-replicas"] != "172.20.255.242" {
		t.Errorf("got %v", got)
	}
}

func lbSvc(name, typ, ip string) map[string]any {
	s := map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"type": typ}}
	if ip != "" {
		s["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": ip}}}}
	}
	return s
}

// Host names and replication's per-pod names both reach the stack DNS.
func TestK3DPublishedHostsInDNS(t *testing.T) {
	cfg, _ := json.Marshal(k3dConfig{TLSHosts: map[string]string{"cluster1-haproxy": "172.20.255.241"},
		ReplHosts: map[string]string{"cluster1-pxc-0": "172.20.255.248"}})
	recs := k3dReplDNSRecords([]Deployment{{NodeID: "k1", Config: cfg}}, map[string]string{"k1": "k3d"})
	if len(recs) != 2 {
		t.Fatalf("got %v", recs)
	}
	z := buildDNSZones("example.net", 1, "172.20.0.0/16", recs, nil)
	if !strings.Contains(z.Forward.Text, "cluster1-haproxy IN A 172.20.255.241") ||
		!strings.Contains(z.Reverse[0].Text, "241.255 IN PTR cluster1-haproxy.example.net.") {
		t.Errorf("forward:\n%s\nreverse:\n%s", z.Forward.Text, z.Reverse[0].Text)
	}
}

func TestK3DTLSHostIssues(t *testing.T) {
	cat := OperatorCatalog{"psmdb": {Latest: "1.23.1", Versions: []string{"1.23.1", "1.21.0"}}, "pxc": {Latest: "1.20.0", Versions: []string{"1.20.0"}}}
	base := designFrame{ID: "f1", Type: "k3d", Label: "k3d-00", K3DOperator: "pxc", K3DCertManager: true, K3DTLSHosts: true}
	if iss := k3dTLSHostIssues(base, designDoc{Frames: []designFrame{base}}, cat, false); len(iss) != 0 {
		t.Errorf("PXC with cert-manager: want silence, got %v", iss)
	}
	twin := with(base, func(f *designFrame) { f.ID = "f2" })
	if iss := k3dTLSHostIssues(base, designDoc{Frames: []designFrame{base, twin}}, cat, false); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("two clusters publishing the same names must be refused, got %v", iss)
	}
	noCM := with(base, func(f *designFrame) { f.K3DCertManager = false })
	if iss := k3dTLSHostIssues(noCM, designDoc{}, cat, false); len(iss) != 1 {
		t.Errorf("without cert-manager: want one warning, got %v", iss)
	}
	mongo := with(base, func(f *designFrame) {
		f.K3DOperator = "psmdb"
		f.K3DOperatorVer = "1.23.1"
		f.K3DExposeReplset = "loadbalancer"
	})
	if iss := k3dTLSHostIssues(mongo, designDoc{}, cat, false); len(iss) != 0 {
		t.Errorf("PSMDB 1.23.1 on a LoadBalancer: want silence, got %v", iss)
	}
	for name, f := range map[string]designFrame{
		"old psmdb":    with(mongo, func(f *designFrame) { f.K3DOperatorVer = "1.21.0" }),
		"sharded":      with(mongo, func(f *designFrame) { f.K3DSharding = true }),
		"clusterip rs": with(mongo, func(f *designFrame) { f.K3DExposeReplset = "clusterip" }),
	} {
		if iss := k3dTLSHostIssues(f, designDoc{}, cat, false); len(iss) != 1 || iss[0].Level != "warning" {
			t.Errorf("%s: want one warning, got %v", name, iss)
		}
	}
}
