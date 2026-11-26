package main

import (
	"net"
	"testing"
)

func TestVMAddresses(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.125.226.0/24")
	ips := vmAddresses(subnet)
	if len(ips) != vmAddrCount || ips[0].String() != "10.125.226.127" || ips[len(ips)-1].String() != "10.125.226.112" {
		t.Fatalf("VM addresses %v: want .127 down to .112, the top of Docker's half", ips)
	}
	aux := vmAuxAddresses(subnet)
	if len(aux) != vmAddrCount || aux["dbcanvas-vm-0"] != "10.125.226.127" {
		t.Fatalf("aux addresses %v", aux)
	}
	ip, ok := pickVMAddress(subnet, map[string]bool{"10.125.226.127": true, "10.125.226.126": true})
	if !ok || ip != "10.125.226.125" {
		t.Fatalf("picked %s %v", ip, ok)
	}
	full := map[string]bool{}
	for _, ip := range ips {
		full[ip.String()] = true
	}
	if _, ok := pickVMAddress(subnet, full); ok {
		t.Fatal("picked an address from a full range")
	}
	_, small, _ := net.ParseCIDR("10.0.0.0/28")
	if vmAddresses(small) != nil {
		t.Fatal("a tiny network gave up addresses")
	}
}
