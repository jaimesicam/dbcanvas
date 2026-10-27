package main

import (
	"strings"
	"testing"
)

func TestBuildDNSZonesAutomaticOnly(t *testing.T) {
	z := buildDNSZones("example.net", 7, "172.20.0.0/16",
		[]dnsRecord{{"intranet", "172.20.0.2"}, {"pmm", "172.20.0.5"}}, nil)
	for _, want := range []string{"intranet IN A 172.20.0.2\n", "pmm IN A 172.20.0.5\n", "( 7 3600"} {
		if !strings.Contains(z.Forward.Text, want) {
			t.Errorf("forward zone lacks %q:\n%s", want, z.Forward.Text)
		}
	}
	if len(z.Reverse) != 1 || z.Reverse[0].Zone != "20.172.in-addr.arpa" || z.Reverse[0].File != dnsReverseFile {
		t.Fatalf("reverse zones = %+v, want the stack network's only", z.Reverse)
	}
	if !strings.Contains(z.Reverse[0].Text, "5.0 IN PTR pmm.example.net.\n") {
		t.Errorf("reverse zone lacks pmm's PTR:\n%s", z.Reverse[0].Text)
	}
}

func TestBuildDNSZonesCustomOverrides(t *testing.T) {
	auto := []dnsRecord{{"intranet", "172.20.0.2"}, {"pmm", "172.20.0.5"}, {"ps01", "172.20.0.6"}}
	custom := []dnsCustomRecord{
		{Type: "A", Name: "pmm", Value: "10.1.2.3", PTR: true},          // replaces pmm's A, adds a PTR in 2.1.10
		{Type: "CNAME", Name: "db", Value: "ps01.example.net"},          // a second name
		{Type: "PTR", Name: "172.20.0.6", Value: "mysql-prod.corp.lan"}, // replaces ps01's PTR
		{Type: "A", Name: "*.apps", Value: "172.20.0.9"},
	}
	z := buildDNSZones("example.net", 1, "172.20.0.0/16", auto, custom)
	fwd := z.Forward.Text
	if strings.Contains(fwd, "pmm IN A 172.20.0.5") {
		t.Errorf("the automatic pmm record survived its override:\n%s", fwd)
	}
	for _, want := range []string{"pmm IN A 10.1.2.3\n", "db IN CNAME ps01.example.net.\n", "*.apps IN A 172.20.0.9\n", "ps01 IN A 172.20.0.6\n"} {
		if !strings.Contains(fwd, want) {
			t.Errorf("forward zone lacks %q:\n%s", want, fwd)
		}
	}
	if len(z.Reverse) != 2 {
		t.Fatalf("want the stack zone plus 2.1.10.in-addr.arpa, got %+v", z.Reverse)
	}
	stackRev, extra := z.Reverse[0].Text, z.Reverse[1]
	if strings.Contains(stackRev, "ps01.example.net") || !strings.Contains(stackRev, "6.0 IN PTR mysql-prod.corp.lan.\n") {
		t.Errorf("ps01's PTR was not replaced:\n%s", stackRev)
	}
	// pmm's old address keeps its automatic PTR: nothing custom claims 172.20.0.5.
	if !strings.Contains(stackRev, "5.0 IN PTR pmm.example.net.\n") {
		t.Errorf("pmm's container address lost its PTR:\n%s", stackRev)
	}
	if extra.Zone != "2.1.10.in-addr.arpa" || extra.File != dnsExtraRevPrefix+"2.1.10.in-addr.arpa" ||
		!strings.Contains(extra.Text, "3 IN PTR pmm.example.net.\n") {
		t.Errorf("extra reverse zone = %+v", extra)
	}
	conf := namedConf(z, "172.20.0.2")
	for _, want := range []string{`zone "example.net"`, `zone "20.172.in-addr.arpa"`, `zone "2.1.10.in-addr.arpa" IN { type master; file "/var/named/dbcanvas.rev.2.1.10.in-addr.arpa"; };`} {
		if !strings.Contains(conf, want) {
			t.Errorf("named.conf lacks %q:\n%s", want, conf)
		}
	}
}

// An address outside a /16 stack zone used to be written into it under the wrong owner name.
func TestBuildDNSZonesAutoOutsideStackNetwork(t *testing.T) {
	z := buildDNSZones("example.net", 1, "172.20.0.0/16", []dnsRecord{{"lb", "192.168.5.10"}}, nil)
	if strings.Contains(z.Reverse[0].Text, "lb.example.net") {
		t.Errorf("an outside address landed in the stack zone:\n%s", z.Reverse[0].Text)
	}
	if len(z.Reverse) != 2 || z.Reverse[1].Zone != "5.168.192.in-addr.arpa" {
		t.Fatalf("want a 5.168.192 zone, got %+v", z.Reverse)
	}
}

func TestNormalizeDNSRecord(t *testing.T) {
	const d = "example.net"
	ok := []struct {
		in, want dnsCustomRecord
	}{
		{dnsCustomRecord{Type: "a", Name: " PMM.example.net. ", Value: "10.0.0.1"}, dnsCustomRecord{Type: "A", Name: "pmm", Value: "10.0.0.1"}},
		{dnsCustomRecord{Type: "A", Name: "db.site1", Value: "10.0.0.2", PTR: true}, dnsCustomRecord{Type: "A", Name: "db.site1", Value: "10.0.0.2", PTR: true}},
		{dnsCustomRecord{Type: "CNAME", Name: "db", Value: "ps01"}, dnsCustomRecord{Type: "CNAME", Name: "db", Value: "ps01.example.net"}},
		{dnsCustomRecord{Type: "CNAME", Name: "ext", Value: "mysql.corp.lan."}, dnsCustomRecord{Type: "CNAME", Name: "ext", Value: "mysql.corp.lan"}},
		{dnsCustomRecord{Type: "PTR", Name: "10.0.0.3", Value: "legacy", PTR: true}, dnsCustomRecord{Type: "PTR", Name: "10.0.0.3", Value: "legacy.example.net"}},
	}
	for _, c := range ok {
		got, err := normalizeDNSRecord(c.in, d)
		if err != nil || got != c.want {
			t.Errorf("normalize(%+v) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []dnsCustomRecord{
		{Type: "MX", Name: "x", Value: "y"},
		{Type: "A", Name: "intranet", Value: "10.0.0.1"},
		{Type: "A", Name: "example.net", Value: "10.0.0.1"},
		{Type: "A", Name: "bad_name-", Value: "10.0.0.1"},
		{Type: "A", Name: "x", Value: "fe80::1"},
		{Type: "A", Name: "*.apps", Value: "10.0.0.1", PTR: true},
		{Type: "A", Name: "a.*", Value: "10.0.0.1"},
		{Type: "CNAME", Name: "db", Value: "db"},
		{Type: "PTR", Name: "not-an-ip", Value: "x"},
		{Type: "PTR", Name: "10.0.0.1", Value: "*.x"},
	}
	for _, in := range bad {
		if got, err := normalizeDNSRecord(in, d); err == nil {
			t.Errorf("normalize(%+v) accepted it as %+v", in, got)
		}
	}
}

func TestDNSConflict(t *testing.T) {
	recs := []dnsCustomRecord{
		{ID: "1", Type: "A", Name: "web", Value: "10.0.0.1"},
		{ID: "2", Type: "CNAME", Name: "db", Value: "ps01.example.net"},
		{ID: "3", Type: "PTR", Name: "10.0.0.1", Value: "web.example.net"},
	}
	cases := []struct {
		r     dnsCustomRecord
		clash bool
	}{
		{dnsCustomRecord{Type: "A", Name: "web", Value: "10.0.0.2"}, false},                     // round robin
		{dnsCustomRecord{Type: "A", Name: "web", Value: "10.0.0.1"}, true},                      // duplicate
		{dnsCustomRecord{Type: "CNAME", Name: "web", Value: "x.example.net"}, true},             // CNAME beside an A
		{dnsCustomRecord{Type: "A", Name: "db", Value: "10.0.0.3"}, true},                       // A beside a CNAME
		{dnsCustomRecord{ID: "2", Type: "CNAME", Name: "db", Value: "ps02.example.net"}, false}, // editing itself
		{dnsCustomRecord{Type: "PTR", Name: "10.0.0.1", Value: "www.example.net"}, false},       // two PTRs are allowed
		{dnsCustomRecord{Type: "PTR", Name: "10.0.0.1", Value: "web.example.net"}, true},        // duplicate PTR
	}
	for _, c := range cases {
		if got := dnsConflict(c.r, recs) != ""; got != c.clash {
			t.Errorf("dnsConflict(%+v) = %v, want %v", c.r, got, c.clash)
		}
	}
}
