package main

import (
	"context"
	"strconv"
	"testing"
)

func TestOSFromImage(t *testing.T) {
	cases := []struct {
		image, os, ver string
		ok             bool
	}{
		{"dbcanvas-systemd:oraclelinux-9-amd64", "oraclelinux", "9", true},
		{"dbcanvas-systemd:ubuntu-22.04-arm64", "ubuntu", "22.04", true},
		{"percona/pmm-server:3", "", "", false}, // wrong shape (two dashes needed)
		{"noколон", "", "", false},
	}
	for _, c := range cases {
		os_, ver, ok := osFromImage(c.image)
		if ok != c.ok || os_ != c.os || ver != c.ver {
			t.Errorf("osFromImage(%q) = (%q,%q,%v), want (%q,%q,%v)", c.image, os_, ver, ok, c.os, c.ver, c.ok)
		}
	}
}

func TestVagrantBox(t *testing.T) {
	if b, ok := vagrantBox("oraclelinux", "9"); !ok || b != "oraclelinux/9" {
		t.Errorf("oraclelinux/9 -> (%q,%v)", b, ok)
	}
	if b, ok := vagrantBox("oraclelinux", "10"); !ok || b != "oraclelinux/10" {
		t.Errorf("oraclelinux/10 -> (%q,%v)", b, ok)
	}
	if _, ok := vagrantBox("oraclelinux", "7"); ok {
		t.Errorf("oraclelinux/7 is not in the DBCanvas OS matrix and must not resolve")
	}
	if b, ok := vagrantBox("ubuntu", "24.04"); !ok || b != "bento/ubuntu-24.04" {
		t.Errorf("ubuntu/24.04 -> (%q,%v)", b, ok)
	}
	if _, ok := vagrantBox("plan9", "1"); ok {
		t.Errorf("unknown os should not resolve")
	}
	t.Setenv("DBCANVAS_BOX_UBUNTU_24_04", "myorg/noble")
	if b, _ := vagrantBox("ubuntu", "24.04"); b != "myorg/noble" {
		t.Errorf("env override not honored: %q", b)
	}
}

func TestRemoteCommand(t *testing.T) {
	if got := remoteCommand("", []string{"echo", "hi"}, []string{"A=1"}); got != `sudo env 'A=1' 'echo' 'hi'` {
		t.Errorf("root: %q", got)
	}
	if got := remoteCommand("postgres", []string{"psql"}, nil); got != `sudo -u 'postgres' env 'psql'` {
		t.Errorf("as-user: %q", got)
	}
	if got := remoteCommand("root", []string{"ls"}, nil); got != `sudo env 'ls'` {
		t.Errorf("explicit root should not add -u: %q", got)
	}
	// A single quote in an arg must be escaped, not break out.
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote escaping: %q", got)
	}
}

func TestVagrantNetworkAndPortState(t *testing.T) {
	v := &Vagrant{root: t.TempDir()}
	ctx := context.Background()

	if err := v.NetworkEnsure(ctx, "dbcanvas-stack-1"); err != nil {
		t.Fatalf("NetworkEnsure: %v", err)
	}
	if err := v.NetworkEnsure(ctx, "dbcanvas-stack-2"); err != nil {
		t.Fatalf("NetworkEnsure 2: %v", err)
	}
	s1, _ := v.NetworkSubnet(ctx, "dbcanvas-stack-1")
	s2, _ := v.NetworkSubnet(ctx, "dbcanvas-stack-2")
	if s1 == s2 || s1 == "" {
		t.Fatalf("distinct networks must get distinct subnets: %q %q", s1, s2)
	}

	ipA, err := v.allocIP("dbcanvas-stack-1", "vmA")
	if err != nil {
		t.Fatalf("allocIP: %v", err)
	}
	ipB, _ := v.allocIP("dbcanvas-stack-1", "vmB")
	if ipA == ipB {
		t.Fatalf("two VMs got the same IP: %s", ipA)
	}
	if again, _ := v.allocIP("dbcanvas-stack-1", "vmA"); again != ipA {
		t.Fatalf("allocIP not stable on redeploy: %s vs %s", again, ipA)
	}
	if got, _ := v.ContainerIP(ctx, "vmA", "dbcanvas-stack-1"); got != ipA {
		t.Fatalf("ContainerIP = %s, want %s", got, ipA)
	}

	// Auto host ports are unique; explicit ones are honored; both are stable.
	p1 := v.assignHostPort("vmA", 5432, 0)
	p2 := v.assignHostPort("vmB", 5432, 0)
	if p1 == p2 {
		t.Fatalf("auto host ports collided: %d", p1)
	}
	if v.assignHostPort("vmA", 5432, 0) != p1 {
		t.Fatalf("host port not stable")
	}
	if hp := v.assignHostPort("vmC", 3306, 33060); hp != 33060 {
		t.Fatalf("explicit host port not honored: %d", hp)
	}
	if got, _ := v.ContainerPort(ctx, "vmA", "5432/tcp"); got != strconv.Itoa(p1) {
		t.Fatalf("ContainerPort = %s, want %d", got, p1)
	}
}

func TestVagrantUnsupportedTypes(t *testing.T) {
	doc := designDoc{
		Nodes:  []designNode{{ID: "a", Type: "pg"}, {ID: "b", Type: "pmm"}, {ID: "c", Type: "intranet"}},
		Frames: []designFrame{{ID: "f", Type: "k3d"}, {ID: "g", Type: "pxc"}},
	}
	bad := vagrantUnsupportedTypes(doc)
	want := map[string]bool{"pmm": true, "k3d": true}
	if len(bad) != len(want) {
		t.Fatalf("unsupported = %v, want keys %v", bad, want)
	}
	for _, b := range bad {
		if !want[b] {
			t.Errorf("unexpected unsupported type %q", b)
		}
	}
}
