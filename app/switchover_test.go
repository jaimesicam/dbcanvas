package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// switchover_test.go — the SQL a MySQL/MariaDB switch sends, per server version, and what the
// design records afterwards. The switches themselves need live servers and were run against them.

func TestDialectFor(t *testing.T) {
	cases := []struct {
		ver     string
		mariadb bool
		modern  bool
		status  string
		binlog  string
	}{
		{"5.7.44-48-log", false, false, "SHOW SLAVE STATUS FOR CHANNEL ''", "SHOW MASTER STATUS"},
		{"8.0.22", false, false, "SHOW SLAVE STATUS FOR CHANNEL ''", "SHOW MASTER STATUS"},
		{"8.0.42-33", false, true, "SHOW REPLICA STATUS FOR CHANNEL ''", "SHOW MASTER STATUS"},
		{"8.4.5-5", false, true, "SHOW REPLICA STATUS FOR CHANNEL ''", "SHOW BINARY LOG STATUS"},
		{"9.7.2-2", false, true, "SHOW REPLICA STATUS FOR CHANNEL ''", "SHOW BINARY LOG STATUS"},
		{"11.4.5-MariaDB-log", true, false, "SHOW SLAVE STATUS", "SHOW MASTER STATUS"},
	}
	for _, c := range cases {
		d := dialectFor(c.ver, c.mariadb)
		if d.Modern != c.modern || d.status() != c.status || d.binlogStatus() != c.binlog {
			t.Errorf("%s: modern=%v status=%q binlog=%q", c.ver, d.Modern, d.status(), d.binlogStatus())
		}
	}
}

func TestChangeSource(t *testing.T) {
	m := dialectFor("8.4.5", false)
	if got := m.changeSource("db-2.example.net", "repl", "p'w", true, "", 0); !strings.Contains(got, "SOURCE_AUTO_POSITION=1") ||
		!strings.Contains(got, "SOURCE_PASSWORD='p''w'") || !strings.HasSuffix(got, "FOR CHANNEL ''") {
		t.Errorf("8.4 GTID: %s", got)
	}
	if got := m.changeSource("db-2", "repl", "x", false, "binlog.000003", 1234); !strings.Contains(got, "SOURCE_AUTO_POSITION=0, SOURCE_LOG_FILE='binlog.000003', SOURCE_LOG_POS=1234") {
		t.Errorf("8.4 file/pos: %s", got)
	}
	old := dialectFor("5.7.44", false)
	if got := old.changeSource("db-2", "repl", "x", false, "mysql-bin.000002", 99); !strings.HasPrefix(got, "CHANGE MASTER TO") ||
		strings.Contains(got, "PUBLIC_KEY") || !strings.Contains(got, "MASTER_LOG_POS=99") {
		t.Errorf("5.7: %s", got)
	}
	maria := dialectFor("11.4.5-MariaDB", true)
	if got := maria.changeSource("db-2", "repl", "x", true, "", 0); !strings.Contains(got, "MASTER_USE_GTID=slave_pos") || strings.Contains(got, "CHANNEL") {
		t.Errorf("MariaDB GTID: %s", got)
	}
	if got := maria.changeSource("db-2", "repl", "x", false, "mariadb-bin.000004", 342); !strings.Contains(got, "MASTER_LOG_FILE='mariadb-bin.000004', MASTER_LOG_POS=342, MASTER_USE_GTID=no") {
		t.Errorf("MariaDB file/pos: %s", got)
	}
}

func TestPosWaitAndReadOnly(t *testing.T) {
	if got := dialectFor("8.4.5", false).posWait("binlog.000001", 157, 60); got != "SELECT SOURCE_POS_WAIT('binlog.000001', 157, 60, '') AS w" {
		t.Errorf("%s", got)
	}
	if got := dialectFor("5.7.44", false).posWait("b.1", 4, 60); !strings.HasPrefix(got, "SELECT MASTER_POS_WAIT(") {
		t.Errorf("%s", got)
	}
	if got := dialectFor("11.4.5-MariaDB", true).posWait("b.1", 4, 60); got != "SELECT MASTER_POS_WAIT('b.1', 4, 60) AS w" {
		t.Errorf("%s", got)
	}
	if got := dialectFor("8.0.42", false).readOnly(false); got != "SET PERSIST super_read_only=OFF; SET PERSIST read_only=OFF" {
		t.Errorf("%s", got)
	}
	if got := dialectFor("5.7.44", false).readOnly(true); !strings.HasPrefix(got, "SET GLOBAL read_only=ON") {
		t.Errorf("%s", got)
	}
	if got := dialectFor("10.11.10-MariaDB", true).readOnly(true); got != "SET GLOBAL read_only=ON" {
		t.Errorf("%s", got)
	}
}

// The design keeps every canvas field it had; only the two roles swap.
func TestRecordNewPrimary(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	design := `{"view":{"x":1,"y":2,"z":1},"frames":[{"id":"f1","type":"mysql","label":"repl","x":10,"gtid":true}],
"nodes":[{"id":"a","type":"mysql","label":"db-1","frameId":"f1","role":"primary","x":5,"color":"teal"},
{"id":"b","type":"mysql","label":"db-2","frameId":"f1","role":"secondary","x":6},
{"id":"c","type":"mysql","label":"db-3","frameId":"f1","role":"secondary","x":7}],"edges":[]}`
	st, err := app.store.CreateStack("repl", u.ID, "4h", nil, []byte(design))
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	for _, n := range []struct{ id, role, src string }{{"a", "primary", ""}, {"b", "secondary", "db-1.example.net"}, {"c", "secondary", "db-1.example.net"}} {
		cfg, _ := json.Marshal(map[string]any{"role": n.role, "sourceHost": n.src, "readOnly": n.role != "primary"})
		app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.id, State: DeployRunning, ContainerID: "x" + n.id, Config: cfg})
	}
	mem := func(id, label string) swMember {
		return swMember{Node: designNode{ID: id, Label: label}, FQDN: label + ".example.net"}
	}
	topo := swTopo{Kind: "mysql", Members: []swMember{mem("a", "db-1"), mem("b", "db-2"), mem("c", "db-3")}}
	if changed, err := app.recordNewPrimary(st, topo, mem("a", "db-1"), mem("b", "db-2")); err != nil || !changed {
		t.Fatalf("record: changed=%v err=%v", changed, err)
	}
	got, _ := app.store.GetStack(st.ID)
	var doc map[string]any
	json.Unmarshal(got.Design, &doc)
	roles := map[string]string{}
	for _, n := range doc["nodes"].([]any) {
		m := n.(map[string]any)
		roles[m["id"].(string)] = m["role"].(string)
		if m["id"] == "a" && m["color"] != "teal" {
			t.Error("a canvas field the Go structs do not model was lost")
		}
	}
	if roles["a"] != "secondary" || roles["b"] != "primary" || roles["c"] != "secondary" {
		t.Errorf("roles after the switch: %v", roles)
	}
	if doc["view"] == nil || doc["frames"].([]any)[0].(map[string]any)["gtid"] != true {
		t.Error("the rest of the design must be untouched")
	}
	for id, want := range map[string]struct {
		role, src string
		ro        bool
	}{"a": {"secondary", "db-2.example.net", true}, "b": {"primary", "", false}, "c": {"secondary", "db-2.example.net", true}} {
		dep, _ := app.store.GetDeployment(st.ID, id)
		var cfg map[string]any
		json.Unmarshal(dep.Config, &cfg)
		if cfg["role"] != want.role || cfg["sourceHost"] != want.src || cfg["readOnly"] != want.ro {
			t.Errorf("deployment %s: %v", id, cfg)
		}
	}
}

// MariaDB 11+ freezes with read_only=NO_LOCK_NO_ADMIN, which refuses admin accounts too; older
// MariaDB has only ON, which does not.
func TestMariaDBStrictReadOnly(t *testing.T) {
	if got := dialectFor("12.3.3-MariaDB-log", true).readOnly(true); got != "SET GLOBAL read_only=NO_LOCK_NO_ADMIN" {
		t.Errorf("12.3: %s", got)
	}
	if got := dialectFor("11.4.5-MariaDB", true).roValue(); got != "NO_LOCK_NO_ADMIN" {
		t.Errorf("11.4: %s", got)
	}
	if d := dialectFor("10.11.10-MariaDB", true); d.mariadbStrictRO() || d.readOnly(true) != "SET GLOBAL read_only=ON" {
		t.Errorf("10.11 has no NO_LOCK_NO_ADMIN: %s", d.readOnly(true))
	}
	if dialectFor("8.4.5", false).mariadbStrictRO() {
		t.Error("MySQL is not MariaDB")
	}
}
