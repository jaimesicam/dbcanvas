package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func tunedFrame(typ string) designFrame {
	return designFrame{Type: typ, Label: "c1", mysqlTuning: mysqlTuning{InnoDBBufferPoolSize: "2G", InnoDBRedoLogCapacity: "1G",
		SyncBinlog: "0", InnoDBFlushLogAtTrxCommit: "2"}}
}

// The redo log size goes in under the name the server knows: innodb_redo_log_capacity from
// 8.0.30, innodb_log_file_size before that and on MariaDB.
func TestMySQLRedoOption(t *testing.T) {
	for _, c := range []struct {
		f    designFrame
		want string
	}{
		{designFrame{Type: "pxc", PXCMajor: "8.0"}, "innodb_redo_log_capacity"},
		{designFrame{Type: "pxc", PXCMajor: "8.0", PXCVersion: "8.0.29-21.1"}, "innodb_log_file_size"},
		{designFrame{Type: "pxc", PXCMajor: "8.0", PXCVersion: "8.0.30-22.1"}, "innodb_redo_log_capacity"},
		{designFrame{Type: "pxc", PXCMajor: "5.7"}, "innodb_log_file_size"},
		{designFrame{Type: "mysql", PSMajor: "5.7", PSVersion: "5.7.44-48"}, "innodb_log_file_size"},
		{designFrame{Type: "mysql", PSMajor: "8.4"}, "innodb_redo_log_capacity"},
		{designFrame{Type: "innodb", PDPSRepo: "pdps-8.0.19"}, "innodb_log_file_size"},
		{designFrame{Type: "innodb", PDPSRepo: "pdps-84-lts"}, "innodb_redo_log_capacity"},
		{designFrame{Type: "mysqlcerepl", MySQLCEMajor: "8.0", MySQLCEVersion: "8.0.28"}, "innodb_log_file_size"},
		{designFrame{Type: "mysqlceinnodb"}, "innodb_redo_log_capacity"},
		{designFrame{Type: "mariadbgalera", MariaDBMajor: "11.4"}, "innodb_log_file_size"},
		{designFrame{Type: "mariadbrepl"}, "innodb_log_file_size"},
	} {
		if got := mysqlRedoOption(c.f); got != c.want {
			t.Errorf("%s %s%s%s%s: %s, want %s", c.f.Type, c.f.PXCMajor, c.f.PSMajor, c.f.PDPSRepo, c.f.MySQLCEVersion, got, c.want)
		}
	}
}

// Every frame renderer writes the settings into [mysqld], and an unset frame writes none.
func TestMySQLTuningInEveryCnf(t *testing.T) {
	want := []string{"innodb_buffer_pool_size=2G", "sync_binlog=0", "innodb_flush_log_at_trx_commit=2"}
	sec := pxcSecrets{ClusterUser: "u", ClusterPassword: "p"}
	cnfs := map[string]string{
		"pxc":           pxcMyCnf(tunedFrame("pxc"), designNode{}, "pxc01", "example.net", "pxc01.example.net", mysqlVaultOptions("8.4")),
		"mysql":         mysqlMyCnf(tunedFrame("mysql"), "ps01", ""),
		"innodb":        innodbMyCnf(tunedFrame("innodb"), "ps01", "example.net", "g", "s", "groupreplication"),
		"mariadbgalera": mariadbGaleraCnf(tunedFrame("mariadbgalera"), "m1", "example.net", "gcomm://", sec),
		"mariadbrepl":   mariadbReplCnf("oraclelinux", "m1", 1, 1, true) + mysqlTuningCnf(tunedFrame("mariadbrepl")),
	}
	for typ, cnf := range cnfs {
		mysqld := cnf[strings.Index(cnf, "[mysqld]"):]
		if i := strings.Index(mysqld, "\n["); i >= 0 {
			mysqld = mysqld[:i] // up to the next section, such as PXC's [sst]
		}
		for _, w := range append(want, mysqlRedoOption(tunedFrame(typ))+"=1G") {
			if !strings.Contains(mysqld, w+"\n") {
				t.Errorf("%s: [mysqld] is missing %s", typ, w)
			}
		}
	}
	plain := mysqlMyCnf(designFrame{Type: "mysql", PSMajor: "8.0"}, "ps01", "")
	for _, k := range []string{"innodb_buffer_pool_size", "innodb_redo_log_capacity", "innodb_log_file_size", "sync_binlog", "innodb_flush_log_at_trx_commit"} {
		if strings.Contains(plain, k) {
			t.Errorf("an unset frame must leave %s to the server", k)
		}
	}
}

// A bare number from an API client is accepted as well as a string.
func TestCnfValueJSON(t *testing.T) {
	var f designFrame
	if err := json.Unmarshal([]byte(`{"syncBinlog":1,"innodbFlushLogAtTrxCommit":"2","innodbBufferPoolSize":null}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.SyncBinlog != "1" || f.InnoDBFlushLogAtTrxCommit != "2" || f.InnoDBBufferPoolSize != "" {
		t.Errorf("got %+v", f)
	}
}

func TestMySQLTuningIssues(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{
			{ID: "ok", Type: "pxc", Label: "ok", mysqlTuning: mysqlTuning{InnoDBBufferPoolSize: "512M", InnoDBRedoLogCapacity: "1g", SyncBinlog: "100", InnoDBFlushLogAtTrxCommit: "1"}},
			{ID: "bad", Type: "mysql", Label: "bad", mysqlTuning: mysqlTuning{InnoDBBufferPoolSize: "2GB", InnoDBRedoLogCapacity: "lots", SyncBinlog: "-1", InnoDBFlushLogAtTrxCommit: "3"}},
			{ID: "big", Type: "mariadbgalera", Label: "big", mysqlTuning: mysqlTuning{InnoDBBufferPoolSize: "4G"}},
			{ID: "pg", Type: "pgcluster", Label: "pg", mysqlTuning: mysqlTuning{SyncBinlog: "x"}},
		},
		Nodes: []designNode{
			{ID: "b1", Label: "big1", FrameID: "big", MemoryGB: 4},
			// Standalone servers are checked against their own memory.
			{ID: "s1", Type: "ps", Label: "ps1", MemoryGB: 2, mysqlTuning: mysqlTuning{InnoDBBufferPoolSize: "2G", SyncBinlog: "x"}},
			{ID: "s2", Type: "mariadb", Label: "ok", mysqlTuning: mysqlTuning{InnoDBRedoLogCapacity: "256M"}},
			{ID: "s3", Type: "pg", Label: "pg", mysqlTuning: mysqlTuning{SyncBinlog: "x"}},
		},
	}
	errs, warns := 0, 0
	for _, is := range mysqlTuningIssues(doc) {
		if strings.Contains(is.Message, " ok:") || strings.Contains(is.Message, " pg:") {
			t.Errorf("unexpected issue: %s", is.Message)
		}
		switch is.Level {
		case "error":
			errs++
		case "warning":
			warns++
			if !strings.Contains(is.Message, "big1") && !strings.Contains(is.Message, "ps1's") {
				t.Errorf("the memory warning must name the member: %s", is.Message)
			}
		}
	}
	if errs != 5 || warns != 2 {
		t.Errorf("errors=%d warnings=%d, want 5 and 2", errs, warns)
	}
	if mysqlSizeBytes("1g") != 1<<30 || mysqlSizeBytes("512M") != 512<<20 || mysqlSizeBytes("2GB") != -1 {
		t.Error("mysqlSizeBytes")
	}
}

// A standalone server's settings reach its config through the synthetic frame, with the redo
// option its own server knows; an untouched node and its JSON write nothing.
func TestMySQLTuningStandalone(t *testing.T) {
	var n designNode
	if err := json.Unmarshal([]byte(`{"type":"ps","psMajor":"5.7","innodbRedoLogCapacity":"512M","syncBinlog":0}`), &n); err != nil {
		t.Fatal(err)
	}
	f := mysqlTuningNodeFrame(n)
	if got := mysqlTuningCnf(f); got != "innodb_log_file_size=512M\nsync_binlog=0\n" {
		t.Errorf("5.7 standalone = %q", got)
	}
	if got := mysqlRedoOption(mysqlTuningNodeFrame(designNode{Type: "mariadb"})); got != "innodb_log_file_size" {
		t.Errorf("MariaDB standalone redo = %s", got)
	}
	if got := mysqlRedoOption(mysqlTuningNodeFrame(designNode{Type: "mysqlce", MySQLCEMajor: "8.4"})); got != "innodb_redo_log_capacity" {
		t.Errorf("MySQL 8.4 standalone redo = %s", got)
	}
	var plain designNode
	if err := json.Unmarshal([]byte(`{"type":"ps","psMajor":"8.0"}`), &plain); err != nil {
		t.Fatal(err)
	}
	if got := mysqlTuningCnf(mysqlTuningNodeFrame(plain)); got != "" {
		t.Errorf("an untouched node must keep the server defaults, wrote %q", got)
	}
	b, _ := json.Marshal(designNode{mysqlTuning: mysqlTuning{SyncBinlog: "1"}})
	if !strings.Contains(string(b), `"syncBinlog":"1"`) {
		t.Errorf("the embedded fields must marshal at the top level: %s", b)
	}
}
