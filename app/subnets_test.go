package main

import (
	"net"
	"testing"
)

func TestPickSubnet(t *testing.T) {
	space := mustCIDRs([]string{"10.0.0.0/22"}) // four /24s
	avoid := mustCIDRs([]string{"10.0.1.0/24", "10.0.2.128/25"})
	got := map[string]bool{}
	for _, seed := range []string{"dbcanvas-ana", "dbcanvas-ben", "dbcanvas-cy", "dbcanvas-dee", "dbcanvas-eve"} {
		n, ok := pickSubnet(space, avoid, seed)
		if !ok {
			t.Fatalf("%s: nothing found", seed)
		}
		if ones, _ := n.Mask.Size(); ones != 24 {
			t.Errorf("%s: %s is not a /24", seed, n)
		}
		for _, a := range avoid {
			if overlaps(n, a) {
				t.Errorf("%s: %s overlaps %s", seed, n, a)
			}
		}
		got[n.String()] = true
		// The same user gets the same answer.
		if again, _ := pickSubnet(space, avoid, seed); again.String() != n.String() {
			t.Errorf("%s: %s then %s", seed, n, again)
		}
	}
	if len(got) < 2 {
		t.Errorf("every user started in the same place: %v", got)
	}
	for k := range got {
		if k != "10.0.0.0/24" && k != "10.0.3.0/24" {
			t.Errorf("picked %s, which is not free", k)
		}
	}
	if _, ok := pickSubnet(space, mustCIDRs([]string{"10.0.0.0/22"}), "x"); ok {
		t.Error("found room in a full range")
	}
}

func TestSubnetSearchSpace(t *testing.T) {
	t.Setenv("DBCANVAS_NETWORK_RANGE", "")
	if s, err := subnetSearchSpace(); err != nil || len(s) != 1 || s[0].String() != "10.0.0.0/8" {
		t.Errorf("default: %v %v", s, err)
	}
	t.Setenv("DBCANVAS_NETWORK_RANGE", "10.20.0.0/16, 10.30.0.0/20")
	if s, err := subnetSearchSpace(); err != nil || len(s) != 2 {
		t.Errorf("two ranges: %v %v", s, err)
	}
	for _, bad := range []string{"nonsense", "10.1.1.0/25"} {
		t.Setenv("DBCANVAS_NETWORK_RANGE", bad)
		if _, err := subnetSearchSpace(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLowerHalfAndGateway(t *testing.T) {
	_, n, _ := net.ParseCIDR("10.7.9.0/24")
	if lowerHalf(n).String() != "10.7.9.0/25" || firstHost(n).String() != "10.7.9.1" {
		t.Errorf("%s %s", lowerHalf(n), firstHost(n))
	}
}

// In a /24 stack network the MetalLB blocks stay in the upper half, where Docker assigns no
// container: fifteen of them (the top two addresses stay free), and not a sixteenth.
func TestMetalLBBlocksInASlash24(t *testing.T) {
	var taken [][2]uint32
	for i := 0; i < 15; i++ {
		r, err := pickMetalLBBlock("10.7.9.0/24", taken, "", i)
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		lo, hi, _ := parseRange(r)
		if lo < ipToU32(net.ParseIP("10.7.9.128").To4()) || hi > ipToU32(net.ParseIP("10.7.9.254").To4()) {
			t.Fatalf("block %d (%s) leaves the upper half", i, r)
		}
		taken = append(taken, [2]uint32{lo, hi})
	}
	if r, err := pickMetalLBBlock("10.7.9.0/24", taken, "", 15); err == nil {
		t.Errorf("a sixteenth block: %s", r)
	}
}
