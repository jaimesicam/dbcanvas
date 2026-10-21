package main

import (
	"strings"
	"testing"
)

func TestPCSMPairIssue(t *testing.T) {
	rs := func(id, major string) pcsmEndpoint { return pcsmEndpoint{ID: id, Topology: "replset", Major: major} }
	sh := func(id, major string) pcsmEndpoint { return pcsmEndpoint{ID: id, Topology: "sharded", Major: major} }
	for _, c := range []struct {
		name     string
		src, dst pcsmEndpoint
		ver      string
		wants    string // "" → supported
	}{
		{"same major rs", rs("a", "8.0"), rs("b", "8.0"), "1.0.0", ""},
		{"upgrade 6 to 8", rs("a", "6.0"), rs("b", "8.0"), "1.0.0", ""},
		{"upgrade 7 to 8", rs("a", "7.0"), rs("b", "8.0"), "1.0.0", ""},
		{"downgrade", rs("a", "8.0"), rs("b", "7.0"), "1.0.0", "older major"},
		{"unsupported major", rs("a", "5.0"), rs("b", "5.0"), "1.0.0", "6.0, 7.0 and 8.0"},
		{"unlisted path", rs("a", "5.0"), rs("b", "8.0"), "1.0.0", "no 5.0 → 8.0 path"},
		{"same cluster", rs("a", "8.0"), rs("a", "8.0"), "1.0.0", "same cluster"},
		{"rs into sharded", rs("a", "8.0"), sh("b", "8.0"), "1.0.0", "only to a replica set"},
		{"sharded to sharded", sh("a", "8.0"), sh("b", "8.0"), "1.0.0", ""},
		{"sharded into rs on 1.0", sh("a", "8.0"), rs("b", "8.0"), "1.0.0", ""},
		{"sharded into rs on 0.9", sh("a", "8.0"), rs("b", "8.0"), "0.9.0", "needs PCSM 1.0.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := pcsmPairIssue(c.src, c.dst, c.ver)
			if c.wants == "" && got != "" || c.wants != "" && !strings.Contains(got, c.wants) {
				t.Fatalf("pcsmPairIssue = %q, want %q", got, c.wants)
			}
		})
	}
}

func TestPCSMURI(t *testing.T) {
	set := scTarget{Hosts: []string{"a.example.net", "b.example.net"}, Port: 27017, ReplicaSet: "rs0", Role: "primary"}
	got := pcsmURI(set, "pcsm-source", "p@ss/word")
	want := "mongodb://pcsm-source:p%40ss%2Fword@a.example.net:27017,b.example.net:27017/?authSource=admin&replicaSet=rs0"
	if got != want {
		t.Errorf("replica set URI\n got %s\nwant %s", got, want)
	}
	router := scTarget{Host: "mongos.example.net", Port: 27017, ReplicaSet: "", Role: "router"}
	if got := pcsmURI(router, "u", "p"); got != "mongodb://u:p@mongos.example.net:27017/?authSource=admin" {
		t.Errorf("router URI: %s", got)
	}
}

func TestPCSMNamespaces(t *testing.T) {
	got, err := pcsmNamespaces([]string{"shop.*, crm.customers", "logs.app"})
	if err != nil || strings.Join(got, ",") != "shop.*,crm.customers,logs.app" {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"shop", ".x", "a.b;reboot", "a.$where"} {
		if _, err := pcsmNamespaces([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestPCSMParseState(t *testing.T) {
	out := "ACTIVE=active\n---STATUS\n{\"ok\":true,\"state\":\"running\"}\n\n---LOG\n\x1b[90m2026-10-20T23:54:29Z\x1b[0m \x1b[32mINF\x1b[0m cloned\n\n"
	active, status, lines := pcsmParseState(out)
	if active != "active" || string(status) != `{"ok":true,"state":"running"}` {
		t.Errorf("active %q status %s", active, status)
	}
	if len(lines) != 1 || lines[0] != "2026-10-20T23:54:29Z INF cloned" {
		t.Errorf("log lines %q — colour escapes should be gone", lines)
	}
	// A node where pcsm is down prints no JSON; that is no status, not a parse error.
	if _, st, _ := pcsmParseState("ACTIVE=failed\n---STATUS\n\n---LOG\n"); st != nil {
		t.Errorf("status %s, want none", st)
	}
}

func TestK3DClusterSyncIssues(t *testing.T) {
	doc := designDoc{Frames: []designFrame{
		{ID: "rs8", Type: "psmrs", Label: "rs8", PSMDBMajor: "8.0"},
		{ID: "rs7", Type: "psmrs", Label: "rs7", PSMDBMajor: "7.0"},
		{ID: "sh8", Type: "psmdb", Label: "sh8", PSMDBMajor: "8.0"},
	}}
	cat := OperatorCatalog{}
	k := func(ver, src string, sharded, search bool) designFrame {
		return designFrame{Type: "k3d", Label: "k", K3DOperator: "psmdb", K3DOperatorVer: ver, K3DClusterSync: true,
			K3DClusterSyncSource: src, K3DSharding: sharded, K3DVectorSearch: search}
	}
	if !psmdbHasClusterSync("1.23.0") || psmdbHasClusterSync("1.22.0") || psmdbHasClusterSync("") {
		t.Fatal("psmdbHasClusterSync gate is wrong")
	}
	// Without a catalog the version cannot be resolved — that is reported, not passed.
	if got := k3dClusterSyncIssues(k("1.23.1", "rs8", false, false), doc, cat); len(got) != 1 || !strings.Contains(got[0].Message, "known operator version") {
		t.Errorf("unresolvable version: %v", got)
	}
	for _, c := range []struct {
		name  string
		f     designFrame
		wants string
	}{
		{"rs from rs 8.0", k("1.23.1", "rs8", false, false), ""},
		{"rs from 7.0", k("1.23.1", "rs7", false, false), "same major"},
		{"rs from sharded", k("1.23.1", "sh8", false, false), "only from a replica set"},
		{"sharded from rs", k("1.23.1", "rs8", true, false), "only from a sharded source"},
		{"with vector search", k("1.23.1", "rs8", false, true), "vector search"},
		{"no source", k("1.23.1", "", false, false), "no source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := k3dClusterSyncFrameIssues(c.f, doc)
			if c.wants == "" {
				if len(got) != 0 {
					t.Fatalf("want none, got %v", got)
				}
				return
			}
			found := false
			for _, i := range got {
				found = found || strings.Contains(i.Message, c.wants)
			}
			if !found {
				t.Fatalf("want an issue containing %q, got %v", c.wants, got)
			}
		})
	}
}

// A K3D PSMDB cluster's mongods share the frame's memory, and the cache block merges with the
// encryption block rather than adding a second `configuration:` key.
func TestPSMDBCacheSizing(t *testing.T) {
	if got := psmdbCacheSizeGB(8, false); got != 0.93 {
		t.Errorf("8 GiB replica set: %v, want 0.93", got)
	}
	if got := psmdbCacheSizeGB(8, true); got != 0.46 {
		t.Errorf("8 GiB sharded: %v, want 0.46", got)
	}
	if got := psmdbCacheSizeGB(2, true); got != 0.25 {
		t.Errorf("floor: %v, want 0.25", got)
	}
	plain := psmdbReplsetConfiguration(psmdbOptions{CacheSizeGB: 0.93}, "c-rs0")
	if plain != "configuration: |\n  storage:\n    wiredTiger:\n      engineConfig:\n        cacheSizeGB: 0.93" {
		t.Errorf("cache only:\n%s", plain)
	}
	both := psmdbReplsetConfiguration(psmdbOptions{CacheSizeGB: 0.5, Vault: &k3dVault{VaultFQDN: "bao", Mount: "m", TLS: true}}, "c-rs0")
	if strings.Count(both, "configuration:") != 1 || !strings.Contains(both, "  security:") || !strings.Contains(both, "cacheSizeGB: 0.5") {
		t.Errorf("vault + cache:\n%s", both)
	}
	if psmdbReplsetConfiguration(psmdbOptions{}, "c-rs0") != "" {
		t.Error("nothing to add should add nothing")
	}
}

func TestPSMDBClusterSyncEnv(t *testing.T) {
	env := psmdbClusterSyncEnv(4)
	for _, want := range []string{"PCSM_CLONE_NUM_INSERT_WORKERS\n    value: \"8\"", "PCSM_CLONE_NUM_READ_WORKERS\n    value: \"2\"", "PCSM_REPL_NUM_WORKERS\n    value: \"4\""} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	if !strings.Contains(psmdbClusterSyncProbes, "timeoutSeconds: 30") {
		t.Error("the probes no longer give /status room to answer during a clone")
	}
}

// The CR the deploy applies is the one the tab's instructions print: built once.
func TestK3DClusterSyncCR(t *testing.T) {
	cr := k3dClusterSyncCR("lab-sync", "lab", psmdbClusterSyncImage, "__SOURCE_URI__", 4)
	for _, want := range []string{"kind: PerconaServerMongoDBClusterSync", "clusterName: lab", "uri: __SOURCE_URI__",
		"credentialsSecret: lab-sync-source", "livenessProbe:", "PCSM_CLONE_NUM_INSERT_WORKERS", "mode: running"} {
		if !strings.Contains(cr, want) {
			t.Errorf("CR lacks %q:\n%s", want, cr)
		}
	}
	if k3dTargetAdminPod(k3dConfig{ClusterName: "lab"}) != [2]string{"lab-rs0-0", "mongod"} ||
		k3dTargetAdminPod(k3dConfig{ClusterName: "lab", Sharding: true}) != [2]string{"lab-mongos-0", "mongos"} {
		t.Error("the state database is reached through the wrong pod")
	}
}
