package main

import "testing"

// proxy_mysqlce_test.go — ProxySQL and HAProxy in front of a MySQL Community replication
// cluster. Both resolve it as a "mysql" backend, so every replication path applies: the
// read_only health check, the rwsplit/primary modes, the authentication-plugin checks.

func mysqlceProxyDoc() designDoc {
	return designDoc{
		Nodes: []designNode{
			{ID: "m1", Type: "mysqlcerepl", FrameID: "ce", Label: "mysqlce-01", Role: "primary"},
			{ID: "m2", Type: "mysqlcerepl", FrameID: "ce", Label: "mysqlce-02", Role: "secondary"},
			{ID: "hap", Type: "haproxy", Label: "haproxy-01"},
			{ID: "psql", Type: "proxysql", Label: "proxysql-01"},
			{ID: "chain", Type: "proxysql", Label: "proxysql-02"},
		},
		Frames: []designFrame{{ID: "ce", Type: "mysqlcerepl", Label: "mysqlce-cluster", MySQLCEMajor: "9.7", MySQLCEVersion: "9.7.4"}},
		Edges: []designEdge{
			{ID: "e1", From: edgeEnd{Node: "ce"}, To: edgeEnd{Node: "hap"}, Type: "directional"},
			{ID: "e2", From: edgeEnd{Node: "ce"}, To: edgeEnd{Node: "psql"}, Type: "directional"},
			{ID: "e3", From: edgeEnd{Node: "psql"}, To: edgeEnd{Node: "chain"}, Type: "directional"},
		},
	}
}

func TestProxiesFrontMySQLCommunity(t *testing.T) {
	doc := mysqlceProxyDoc()
	check := func(who string, f designFrame, kind string, ok bool) {
		t.Helper()
		if !ok {
			t.Fatalf("%s: no backend resolved", who)
		}
		// Kind "mysql" selects the replication paths; the type stays the frame's own so the
		// member lookups find mysqlcerepl members; PSMajor carries the community series.
		if kind != "mysql" || f.Type != "mysqlcerepl" || f.ID != "ce" || f.PSMajor != "9.7" || f.PSVersion != "9.7.4" {
			t.Errorf("%s: kind=%q type=%q id=%q psMajor=%q psVersion=%q", who, kind, f.Type, f.ID, f.PSMajor, f.PSVersion)
		}
	}
	f, kind, ok := haproxyBackend(doc, "hap")
	check("HAProxy", f, kind, ok)
	f, kind, ok = backendFrameForProxySQL(doc, "psql")
	check("ProxySQL", f, kind, ok)
	f, kind, ok = backendFrameForProxySQL(doc, "chain")
	check("chained ProxySQL", f, kind, ok)

	// A 9.7 backend has no mysql_native_password, so a ProxySQL older than 2.6 is refused —
	// which needs the community series to reach the check.
	if got := proxysqlBackendIssues(doc, "ProxySQL node proxysql-01", "psql", "2", "2.5.5", ""); len(got) != 1 {
		t.Errorf("ProxySQL 2.5 in front of MySQL 9.7: %v", got)
	}
	if got := proxysqlBackendIssues(doc, "ProxySQL node proxysql-01", "psql", "2", "", ""); len(got) != 0 {
		t.Errorf("newest ProxySQL in front of MySQL 9.7: %v", got)
	}
}

// Every other frame comes back unchanged, with its own type as the kind.
func TestReplBackendFrameLeavesOthersAlone(t *testing.T) {
	for _, typ := range []string{"pxc", "mysql", "patroni", "repmgr", "pgstream", "spock"} {
		f, kind := replBackendFrame(designFrame{ID: "x", Type: typ, PSMajor: "8.0"})
		if kind != typ || f.Type != typ || f.PSMajor != "8.0" {
			t.Errorf("%s: kind=%q type=%q psMajor=%q", typ, kind, f.Type, f.PSMajor)
		}
	}
}
