package main

import "testing"

func TestCompareSettings(t *testing.T) {
	members := []designNode{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	settings := []map[string]string{
		{"innodb_buffer_pool_size": "134217728", "server_id": "1", "read_only": "OFF", "max_connections": "151", "log_error": "/var/log/<host>.err"},
		{"innodb_buffer_pool_size": "268435456", "server_id": "2", "read_only": "ON", "max_connections": "151", "log_error": "/var/log/<host>.err"},
		nil, // did not answer
	}
	rows := compareSettings(members, settings)
	if len(rows) != 2 || rows[0].Name != "innodb_buffer_pool_size" || rows[0].Expected || rows[1].Name != "read_only" || !rows[1].Expected {
		t.Fatalf("rows = %+v", rows)
	}
	if _, ok := rows[0].Values["c"]; ok {
		t.Fatal("a member that did not answer is not a value")
	}
	// A setting only one member has is drift too.
	rows = compareSettings(members[:2], []map[string]string{{"x": "1"}, {}})
	if len(rows) != 1 || rows[0].Values["b"] != "(not set)" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestMemberHealthy(t *testing.T) {
	lag := func(f float64) *float64 { return &f }
	cases := []struct {
		kind string
		r    *liveRole
		want bool
	}{
		{"mysql", &liveRole{Role: "replica", LagSec: lag(0)}, true},
		{"mysql", &liveRole{Role: "replica", LagSec: lag(60)}, false},
		{"mysql", &liveRole{Role: "standalone"}, false},
		{"mysql", &liveRole{Role: "replica", Problems: []string{"x"}}, false},
		{"galera", &liveRole{Role: "member", State: "Joined"}, false},
		{"galera", &liveRole{Role: "member", State: "Synced"}, true},
		{"mongo", &liveRole{Role: "member"}, false},
		{"gr", &liveRole{Role: "secondary"}, true},
		{"patroni", &liveRole{Down: true}, false},
		{"patroni", nil, false},
	}
	for _, c := range cases {
		if got := memberHealthy(c.kind, c.r); got != c.want {
			t.Errorf("%s %+v = %v", c.kind, c.r, got)
		}
	}
}

func TestCollapsePlugins(t *testing.T) {
	members := []designNode{{ID: "a"}, {ID: "b"}}
	a := map[string]string{"max_connections": "151"}
	b := map[string]string{"max_connections": "200"}
	for _, k := range []string{"clone_a", "clone_b", "clone_c", "clone_d"} {
		a[k] = "1"
	}
	rows := compareSettings(members, []map[string]string{a, b})
	if len(rows) != 2 || rows[0].Name != "clone_* (4 settings — a plugin or component loaded on some members only)" && rows[1].Name != "clone_* (4 settings — a plugin or component loaded on some members only)" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows := compareSettings(members, []map[string]string{{"x": "1"}, {"x": "1"}}); rows == nil || len(rows) != 0 {
		t.Fatalf("no drift must be an empty list, got %#v", rows)
	}
}
