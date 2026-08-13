package main

import (
	"strings"
	"testing"
)

// Network conditions exist to make a node behave badly, so the clamps are the
// safety rail: a typo must not sever a cluster nobody meant to sever.

func TestClampNetemBounds(t *testing.T) {
	s := clampNetem(-5, -5, -5, -5, false, nil)
	if s.LatencyMS != 0 || s.JitterMS != 0 || s.LossPct != 0 || s.RateMbit != 0 {
		t.Errorf("negatives should clamp to zero, got %+v", s)
	}
	if !s.Empty() {
		t.Error("an all-zero spec must be Empty, or the node gets shaped with nothing")
	}
	over := clampNetem(99999, 99999, 999, 99999, false, nil)
	if over.LatencyMS != netemMaxLatencyMS {
		t.Errorf("latency = %d, want %d", over.LatencyMS, netemMaxLatencyMS)
	}
	if over.LossPct != netemMaxLossPct {
		t.Errorf("loss = %v, want %d", over.LossPct, netemMaxLossPct)
	}
	if over.RateMbit != netemMaxRateMbit {
		t.Errorf("rate = %d, want %d", over.RateMbit, netemMaxRateMbit)
	}
}

// TestClampNetemJitterNeverExceedsLatency pins the one clamp that is about
// correctness rather than sanity. netem draws each packet's delay from
// latency ± jitter; a jitter above the latency produces negative delays, which
// reorder packets. TCP reads reordering as loss, so the link stops modelling
// what was asked for and starts modelling something else entirely.
func TestClampNetemJitterNeverExceedsLatency(t *testing.T) {
	for _, c := range []struct{ lat, jit, want int }{
		{100, 20, 20},
		{100, 100, 100},
		{100, 500, 100},
		{10, 400, 10},
		{0, 50, 0},
	} {
		got := clampNetem(c.lat, c.jit, 0, 0, false, nil)
		if got.JitterMS != c.want {
			t.Errorf("latency %d jitter %d → %d, want %d", c.lat, c.jit, got.JitterMS, c.want)
		}
		if got.JitterMS > got.LatencyMS {
			t.Errorf("jitter %d exceeds latency %d — packets would reorder",
				got.JitterMS, got.LatencyMS)
		}
	}
}

// TestNetemArgsShape checks the string handed to tc. A malformed netem argument
// is rejected by tc with a usage message, and since the whole thing runs as one
// script that means no shaping at all.
func TestNetemArgsShape(t *testing.T) {
	for _, c := range []struct {
		spec netemSpec
		want string
	}{
		{netemSpec{}, ""},
		{netemSpec{LatencyMS: 100}, "delay 100ms"},
		{netemSpec{LatencyMS: 100, JitterMS: 20}, "delay 100ms 20ms distribution normal"},
		{netemSpec{LossPct: 10}, "loss 10%"},
		{netemSpec{LossPct: 2.5}, "loss 2.5%"},
		{netemSpec{LatencyMS: 50, JitterMS: 5, LossPct: 1}, "delay 50ms 5ms distribution normal loss 1%"},
		// A bandwidth cap alone is htb's job; netem must contribute nothing.
		{netemSpec{RateMbit: 10}, ""},
	} {
		if got := c.spec.netemArgs(); got != c.want {
			t.Errorf("%+v → %q, want %q", c.spec, got, c.want)
		}
	}
}

func TestNetemRateArg(t *testing.T) {
	if got := (netemSpec{}).rateArg(); got != "10gbit" {
		t.Errorf("no cap should be the unshaped rate, got %q", got)
	}
	if got := (netemSpec{RateMbit: 100}).rateArg(); got != "100mbit" {
		t.Errorf("rateArg = %q, want 100mbit", got)
	}
}

// TestNetemPortsIncludeClusterTraffic is the point of the whole feature for a
// synchronous cluster: impairing only the client port models a bad link to the
// application, which is a different experiment from impairing the link between
// members. Galera's group communication, IST and SST ports must all be shaped.
func TestNetemPortsIncludeClusterTraffic(t *testing.T) {
	for _, typ := range []string{"pxc", "mariadbgalera"} {
		ports := netemPortsFor(typ)
		for _, want := range []int{3306, 4444, 4567, 4568} {
			found := false
			for _, p := range ports {
				if p == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s must shape port %d — without it flow control and eviction "+
					"cannot be produced", typ, want)
			}
		}
	}
	// Patroni agrees on a primary over its REST API, so a partition there is its
	// own failure mode and has to be reachable by the knob.
	if ports := netemPortsFor("patroni"); len(ports) != 2 || ports[0] != 5432 || ports[1] != 8008 {
		t.Errorf("patroni ports = %v, want [5432 8008]", ports)
	}
	// Node types with no cluster traffic get nothing, and that is what turns the
	// control off for them.
	for _, typ := range []string{"stocksim", "pmm", "intranet", "banana", ""} {
		if netemSupported(typ) {
			t.Errorf("%q should not offer network shaping", typ)
		}
	}
}

func TestNormalisePorts(t *testing.T) {
	got := normalisePorts([]int{4567, 3306, 4567, 0, -1, 70000, 4444})
	want := []int{3306, 4444, 4567}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestNetemEnvClearsWhenEmpty pins the teardown path. An empty spec has to reach
// the script as SPEC="" so it deletes the qdisc — that is the only way a node
// whose conditions were removed on a redeploy loses its shaping.
func TestNetemEnvClearsWhenEmpty(t *testing.T) {
	env := strings.Join((netemSpec{}).netemEnv(""), " ")
	if !strings.Contains(env, "SPEC= ") && !strings.HasSuffix(env, "SPEC=") {
		if strings.Contains(env, "SPEC=on") {
			t.Fatal("an empty spec must not set SPEC=on, or removed conditions never clear")
		}
	}
	on := strings.Join((netemSpec{LatencyMS: 50}).netemEnv(""), " ")
	if !strings.Contains(on, "SPEC=on") {
		t.Error("a non-empty spec must set SPEC=on")
	}
	if !strings.Contains(on, "DEV=eth0") {
		t.Error("the default device should be filled in when none is given")
	}
}

// TestNetemScriptIsIdempotent guards the property that stops delays stacking:
// the script must delete any existing root qdisc before adding one. Applying
// twice without it gives two netem qdiscs and double the latency.
func TestNetemScriptIsIdempotent(t *testing.T) {
	del := strings.Index(netemScript, "tc qdisc del dev")
	add := strings.Index(netemScript, "tc qdisc add dev \"$DEV\" root handle 1: htb")
	if del < 0 || add < 0 {
		t.Fatal("the script must both delete and add the root qdisc")
	}
	if del > add {
		t.Error("the delete must come before the add, or applying twice stacks two netem qdiscs")
	}
	// Both directions have to be filtered: a reply leaves with the cluster port
	// as its source, so matching only dport impairs requests but not responses.
	if !strings.Contains(netemScript, "match ip dport") || !strings.Contains(netemScript, "match ip sport") {
		t.Error("both sport and dport must be matched, or only one direction is shaped")
	}
	// The default class must exist and be unshaped, or shaping the cluster ports
	// would also impair DNS, LDAP and the health checks.
	if !strings.Contains(netemScript, "htb default 10") {
		t.Error("there must be an unshaped default class")
	}
}

func TestDescribeNetem(t *testing.T) {
	if got := describeNetem(netemSpec{}); got != "none" {
		t.Errorf("empty spec = %q, want none", got)
	}
	s := clampNetem(100, 20, 5, 50, false, netemPortsFor("pxc"))
	got := describeNetem(s)
	for _, want := range []string{"100ms latency", "±20ms", "5% loss", "50 Mbit/s", "4567"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeNetem = %q, missing %q", got, want)
		}
	}
	if all := describeNetem(clampNetem(10, 0, 0, 0, true, nil)); !strings.Contains(all, "all traffic") {
		t.Errorf("all-traffic spec should say so, got %q", all)
	}
}

// The canvas has to refuse or warn about the settings that will not do what
// they look like they do.
func TestNetemIssues(t *testing.T) {
	// A node type with no cluster traffic cannot be shaped, and says so rather
	// than accepting a control that does nothing.
	ig := netemIssues(designNode{Label: "sim1", Type: "stocksim", NetLatencyMS: 50})
	if len(ig) != 1 || ig[0].Level != "warning" {
		t.Fatalf("shaping an unsupported type: got %+v, want one warning", ig)
	}
	// Nothing asked for, nothing said.
	if q := netemIssues(designNode{Label: "n1", Type: "pxc"}); len(q) != 0 {
		t.Errorf("an unshaped node should raise nothing, got %+v", q)
	}
	// A valid setting is called out, because it degrades the node on purpose.
	ok := netemIssues(designNode{Label: "n1", Type: "pxc", NetLatencyMS: 100, NetJitterMS: 20})
	if len(ok) != 1 || ok[0].Level != "info" {
		t.Fatalf("valid conditions: got %+v, want one info", ok)
	}
	// Jitter above latency is the correctness trap.
	j := netemIssues(designNode{Label: "n1", Type: "pxc", NetLatencyMS: 10, NetJitterMS: 400})
	if len(j) != 1 || j[0].Level != "warning" || !strings.Contains(j[0].Message, "reorder") {
		t.Fatalf("jitter above latency: got %+v, want a warning naming reordering", j)
	}
	// Jitter with no latency does nothing at all.
	jn := netemIssues(designNode{Label: "n1", Type: "pxc", NetJitterMS: 20})
	if len(jn) != 1 || jn[0].Level != "warning" {
		t.Fatalf("jitter with no latency: got %+v, want one warning", jn)
	}
	// All-traffic is legal but has to be flagged: it can fail provisioning.
	at := netemIssues(designNode{Label: "n1", Type: "pxc", NetLatencyMS: 50, NetAllTraffic: true})
	if len(at) == 0 {
		t.Fatal("all-traffic shaping must be warned about")
	}
	found := false
	for _, i := range at {
		if i.Level == "warning" {
			found = true
		}
	}
	if !found {
		t.Errorf("all-traffic: got %+v, want a warning", at)
	}
}
