package main

import (
	"strings"
	"testing"
)

// A Valkey Cluster is a whole number of shards, 3 to 7 of them, at most 9 nodes.
func TestValkeyClusterShape(t *testing.T) {
	for _, c := range []struct {
		replicas, members int
		ok                bool
	}{
		{0, 3, true}, {0, 7, true}, {0, 2, false}, {0, 8, false},
		{1, 6, true}, {1, 8, true}, {1, 7, false}, {1, 4, false}, {1, 10, false},
		{2, 9, true}, {2, 6, false}, {3, 12, false}, {-1, 3, false},
	} {
		got := valkeyClusterShapeIssue(designFrame{ValkeyReplicas: c.replicas}, c.members)
		if (got == "") != c.ok {
			t.Errorf("%d replicas, %d members: %q", c.replicas, c.members, got)
		}
	}
}

// Sentinel mode is one primary and its replicas: 3 to 9 members, no shards.
func TestValkeySentinelShape(t *testing.T) {
	f := designFrame{ValkeyMode: "sentinel"}
	for members, ok := range map[int]bool{2: false, 3: true, 5: true, 9: true, 10: false} {
		if got := valkeyClusterShapeIssue(f, members); (got == "") != ok {
			t.Errorf("%d members: %q", members, got)
		}
	}
	f.ValkeyReplicas = 1
	if valkeyClusterShapeIssue(f, 3) == "" {
		t.Error("replicas per shard accepted in Sentinel mode")
	}
}

// Sentinel's quorum is a majority, and it names members by host.
func TestValkeySentinelConf(t *testing.T) {
	c := valkeySentinelConf("vk", "valkey-1.example.net", "valkey-2.example.net", "pw", 5)
	for _, want := range []string{"sentinel monitor vk valkey-1.example.net 6379 3\n", "sentinel resolve-hostnames yes\n", "sentinel auth-pass vk pw\n", "port 26379\n", "sentinel announce-ip valkey-2.example.net\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in\n%s", want, c)
		}
	}
	if strings.Index(c, "resolve-hostnames") > strings.Index(c, "sentinel monitor") {
		t.Error("resolve-hostnames must come before the monitor line")
	}
}

// Every Sentinel-mode member announces its host name, the first primary as
// well, and only replicas start with a replicaof line.
func TestValkeySentinelMemberConf(t *testing.T) {
	primary := valkeyConfFile("pw", "example.net", "dc=example,dc=net", false, false, "", "", "valkey-1.example.net")
	if !strings.Contains(primary, "replica-announce-ip valkey-1.example.net\n") || strings.Contains(primary, "replicaof") {
		t.Errorf("primary:\n%s", primary)
	}
	replica := valkeyConfFile("pw", "example.net", "dc=example,dc=net", false, false, "", "valkey-1.example.net", "valkey-2.example.net")
	for _, want := range []string{"replicaof valkey-1.example.net 6379\n", "replica-announce-ip valkey-2.example.net\n"} {
		if !strings.Contains(replica, want) {
			t.Errorf("replica: missing %q in\n%s", want, replica)
		}
	}
}
