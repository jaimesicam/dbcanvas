package main

import (
	"net"
	"strings"
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

// TestVMsToResume: only a stopped VM of a running deployment comes back; one stopped from
// DBCanvas, a broken one, one off the stack network and one of no deployment stay as they are.
func TestVMsToResume(t *testing.T) {
	managed := map[string]limaMeta{
		"dbc-a": {Name: "a", IP: "10.1.0.240"}, // stopped by the machine's restart: resumed
		"dbc-b": {Name: "b", IP: "10.1.0.241"}, // running already
		"dbc-c": {Name: "c", IP: "10.1.0.242"}, // stopped from DBCanvas (deployment stopped)
		"dbc-d": {Name: "d", IP: "10.1.0.243"}, // broken
		"dbc-e": {Name: "e"},                   // never joined a stack network
		"dbc-f": {Name: "f", IP: "10.1.0.244"}, // no deployment
		"dbc-g": {Name: "g", IP: "10.1.0.245"}, // resumed
	}
	all := map[string]limaInstance{
		"dbc-a": {Status: "Stopped"}, "dbc-b": {Status: "Running"}, "dbc-c": {Status: "Stopped"},
		"dbc-d": {Status: "Broken"}, "dbc-e": {Status: "Stopped"}, "dbc-f": {Status: "Stopped"},
		"dbc-g": {Status: "Stopped"},
	}
	running := map[string]bool{"dbc-a": true, "dbc-b": true, "dbc-d": true, "dbc-e": true, "dbc-g": true}
	got := vmsToResume(managed, all, running)
	if strings.Join(got, ",") != "dbc-a,dbc-g" {
		t.Fatalf("vmsToResume = %v, want [dbc-a dbc-g]", got)
	}
}
