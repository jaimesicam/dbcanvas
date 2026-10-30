package main

import "testing"

func TestLiveLBsFromServices(t *testing.T) {
	js := `{"items":[
	 {"metadata":{"name":"db1-haproxy","namespace":"pxc"},"spec":{"type":"LoadBalancer","ports":[{"port":3306},{"port":3309}]},
	  "status":{"loadBalancer":{"ingress":[{"ip":"172.29.0.210"}]}}},
	 {"metadata":{"name":"db1-pxc-0-dbcanvas","namespace":"pxc"},"spec":{"type":"LoadBalancer","ports":[{"port":3306}]},
	  "status":{"loadBalancer":{"ingress":[{"ip":"172.29.0.9"}]}}},
	 {"metadata":{"name":"pending","namespace":"pxc"},"spec":{"type":"LoadBalancer"},"status":{"loadBalancer":{}}},
	 {"metadata":{"name":"kubernetes","namespace":"default"},"spec":{"type":"ClusterIP"},"status":{}}
	]}`
	got := liveLBsFromServices(js)
	if len(got) != 2 {
		t.Fatalf("want the two LoadBalancers with an address, got %+v", got)
	}
	// Ordered by address numerically: .9 before .210.
	if got[0].IP != "172.29.0.9" || got[0].Name != "db1-pxc-0-dbcanvas" || got[1].IP != "172.29.0.210" {
		t.Fatalf("order/contents: %+v", got)
	}
	if len(got[1].Ports) != 2 || got[1].Ports[0] != 3306 || got[1].Namespace != "pxc" {
		t.Fatalf("ports/namespace: %+v", got[1])
	}
	if out := liveLBsFromServices("not json"); len(out) != 0 {
		t.Fatalf("bad json should give none, got %+v", out)
	}
}

func TestIPLess(t *testing.T) {
	if !ipLess("172.29.0.9", "172.29.0.10") || ipLess("172.29.0.10", "172.29.0.9") {
		t.Fatal("numeric order")
	}
	if !ipLess("a", "b") {
		t.Fatal("text fallback")
	}
}
