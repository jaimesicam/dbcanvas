package main

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
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

func TestNodeEngineRouting(t *testing.T) {
	a := &App{docker: &Docker{}, vagrant: &Vagrant{root: t.TempDir()}}
	vagrant := Engine(a.vagrant)
	docker := Engine(a.docker)

	cases := []struct {
		backend, typ string
		want         Engine
	}{
		{BackendVagrant, "pg", vagrant},       // DB node -> VM
		{BackendVagrant, "pxc", vagrant},      // DB cluster frame -> VM
		{BackendVagrant, "intranet", docker},  // DNS/CA hub stays Docker (bind forwards to 127.0.0.11)
		{BackendVagrant, "pmm", docker},       // image-only infra stays Docker in hybrid
		{BackendVagrant, "k3d", docker},       // k3s-in-Docker stays Docker
		{BackendVagrant, "seaweedfs", docker}, //
		{BackendDocker, "pg", docker},         // docker stack -> everything Docker
		{"", "pg", docker},                    // unstamped stack -> Docker
	}
	for _, c := range cases {
		if got := a.nodeEngine(Stack{Backend: c.backend}, c.typ); got != c.want {
			t.Errorf("nodeEngine(%q, %q) routed wrong", c.backend, c.typ)
		}
	}

	// depEngine resolves a node's engine from its type in the design.
	st := Stack{Backend: BackendVagrant, Design: []byte(`{"nodes":[{"id":"n1","type":"pg"},{"id":"n2","type":"pmm"}]}`)}
	if a.depEngine(st, "n1") != vagrant {
		t.Errorf("depEngine n1(pg) should be vagrant")
	}
	if a.depEngine(st, "n2") != docker {
		t.Errorf("depEngine n2(pmm) should be docker")
	}
	if a.depEngine(st, "gone") != docker {
		t.Errorf("depEngine of a removed/unknown node should default to docker")
	}
	// With no vagrant backend on the host, a vagrant stack still routes to Docker.
	aNoVagrant := &App{docker: &Docker{}}
	if aNoVagrant.nodeEngine(Stack{Backend: BackendVagrant}, "pg") != Engine(aNoVagrant.docker) {
		t.Errorf("no vagrant backend -> must fall back to docker")
	}
}

// stampEngine must put the node's engine on the request context so management
// handlers (which read it via engCtx) exec on the right engine: the VM for a hybrid
// stack's DB node, Docker for infra and for docker stacks.
func TestStampEngineOnRequest(t *testing.T) {
	a := &App{docker: &Docker{}, vagrant: &Vagrant{root: t.TempDir()}}
	st := Stack{ID: 1, Backend: BackendVagrant,
		Design: []byte(`{"nodes":[{"id":"db","type":"pg"},{"id":"mon","type":"pmm"}]}`)}

	req := httptest.NewRequest("POST", "/x", nil)
	a.stampEngine(req, st, "db") // pg -> VM
	if got := a.engCtx(req.Context()); got != Engine(a.vagrant) {
		t.Errorf("pg node should stamp the vagrant engine")
	}

	req = httptest.NewRequest("POST", "/x", nil)
	a.stampEngine(req, st, "mon") // pmm stays Docker
	if got := a.engCtx(req.Context()); got != Engine(a.docker) {
		t.Errorf("pmm node should stamp the docker engine")
	}

	// A docker stack always stamps Docker, even for a DB node.
	req = httptest.NewRequest("POST", "/x", nil)
	a.stampEngine(req, Stack{ID: 2, Backend: BackendDocker, Design: st.Design}, "db")
	if got := a.engCtx(req.Context()); got != Engine(a.docker) {
		t.Errorf("docker stack should stamp the docker engine")
	}
}

func TestStackRules(t *testing.T) {
	rules := stackRules("172.20.0.0/16", "192.168.56.0/24", 7)
	if len(rules) != 6 {
		t.Fatalf("want 6 rules, got %d", len(rules))
	}
	joined := func(r iptRule) string { return r.table + " " + strings.Join(r.args, " ") }
	// (1) raw/PREROUTING ACCEPTs bypass Docker 29's direct-routing DROP; (2) filter/
	// DOCKER-USER opens FORWARD both ways; (3) nat/POSTROUTING RETURNs exempt cross-
	// engine traffic from MASQUERADE. All tagged with the stack comment.
	want := []string{
		"raw PREROUTING -s 192.168.56.0/24 -d 172.20.0.0/16 -j ACCEPT",
		"raw PREROUTING -s 172.20.0.0/16 -d 192.168.56.0/24 -j ACCEPT",
		"filter DOCKER-USER -s 192.168.56.0/24 -d 172.20.0.0/16 -j ACCEPT",
		"filter DOCKER-USER -s 172.20.0.0/16 -d 192.168.56.0/24 -j ACCEPT",
		"nat POSTROUTING -s 172.20.0.0/16 -d 192.168.56.0/24 -j RETURN",
		"nat POSTROUTING -s 192.168.56.0/24 -d 172.20.0.0/16 -j RETURN",
	}
	for i, w := range want {
		if !strings.HasPrefix(joined(rules[i]), w) {
			t.Errorf("rule %d = %q, want prefix %q", i, joined(rules[i]), w)
		}
		if !strings.Contains(joined(rules[i]), "--comment dbcanvas-stack-7") {
			t.Errorf("rule %d missing stack comment: %v", i, rules[i])
		}
	}
}

func TestHostOnlyGateway(t *testing.T) {
	cases := []struct{ cidr, want string }{
		{"192.168.56.0/24", "192.168.56.1"},
		{"192.168.60.0/24", "192.168.60.1"},
		{"172.20.0.0/16", "172.20.0.1"},
		{"not-a-cidr", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := hostOnlyGateway(c.cidr); got != c.want {
			t.Errorf("hostOnlyGateway(%q) = %q, want %q", c.cidr, got, c.want)
		}
	}
}

func TestValidCIDR(t *testing.T) {
	for _, ok := range []string{"172.20.0.0/16", "192.168.56.0/24"} {
		if !validCIDR(ok) {
			t.Errorf("validCIDR(%q) should be true", ok)
		}
	}
	for _, bad := range []string{"", "172.20.0.0", "garbage", "192.168.56.0/33"} {
		if validCIDR(bad) {
			t.Errorf("validCIDR(%q) should be false", bad)
		}
	}
}

// reconcileStackRouting / linkStackNetworks must be inert for a docker-only stack or
// a host with no vagrant backend — they must not shell out to iptables at all.
func TestRoutingNoopWithoutHybrid(t *testing.T) {
	a := &App{docker: &Docker{}} // no vagrant backend
	a.reconcileStackRouting(context.Background(), Stack{ID: 1, Backend: BackendVagrant}, nil)
	a.linkStackNetworks(context.Background(), Stack{ID: 1, Backend: BackendVagrant})
	a.unlinkStackNetworks(context.Background(), 1)

	av := &App{docker: &Docker{}, vagrant: &Vagrant{root: t.TempDir()}}
	av.reconcileStackRouting(context.Background(), Stack{ID: 2, Backend: BackendDocker}, nil)
	av.linkStackNetworks(context.Background(), Stack{ID: 2, Backend: BackendDocker})
}
