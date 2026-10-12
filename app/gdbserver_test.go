package main

import (
	"strings"
	"testing"
)

// The extractor prints exactly one line of JSON behind a sentinel, on a console stream that also
// carries gdb's library-load chatter and the script's own notes. gdbParseServerState has to pick
// that one line out and ignore everything else, so the fixtures here wrap real captured output
// around it — a run against a PS 8.4.11 core and a PXC 8.4.11 core — rather than a bare JSON blob.

// The JSON these fixtures carry is the shape captured live from the two cores this feature was
// built against: the PS lock-pileup core (an open transaction holding a row lock, an UPDATE
// waiting on it, two statements queued behind a metadata lock) and the PXC holder core.
const psServerStateLine = `<<CDA_SERVERSTATE>>{"meta":{"product":"ps","version":"8.4.11-11","startTime":1791811976},` +
	`"sessions":[` +
	`{"threadId":9,"command":"Sleep","db":"stocksim","query":"","state":""},` +
	`{"threadId":74,"command":"Query","db":"stocksim","query":"SELECT SLEEP(3600) AS holder_trx","state":"User sleep","gdbThread":41},` +
	`{"threadId":75,"command":"Query","db":"stocksim","query":"UPDATE lab_parking SET holds=holds+1 WHERE id=1","state":"updating","gdbThread":42},` +
	`{"threadId":76,"command":"Query","db":"stocksim","query":"ALTER TABLE lab_parking COMMENT='mdl_waiter'","state":"Waiting for table metadata lock","gdbThread":43}],` +
	`"innodb":{"historyListLength":90,"flushedLsn":42047844,"checkpointLsn":40060354,"transactions":[` +
	`{"id":29863,"state":"ACTIVE","rowsModified":0,"threadId":75,"op":"starting index read","waiting":true,"blockedByTrx":29807,"gdbThread":42},` +
	`{"id":29807,"state":"ACTIVE","rowsModified":2,"threadId":74,"op":"","waiting":false,"gdbThread":41}]},` +
	`"binlog":{"file":"./binlog.000002","position":1284718},` +
	`"errorLog":[{"ts":1791811439940273,"prio":0,"code":"MY-015017","subsys":"Server","message":"MySQL Server Initialization - start."},` +
	`{"ts":1791812086000000,"prio":1,"code":"MY-013183","subsys":"InnoDB","message":"got signal 11"}],` +
	`"wsrep":null,"notes":[]}`

func TestGDBParseServerState_PS(t *testing.T) {
	out := "Reading symbols from /sysroot/mysqld...\n" +
		"[New LWP 13662]\n" +
		psServerStateLine + "\n" +
		"(gdb) quit\n"
	st, err := gdbParseServerState(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.Meta.Product != "ps" || st.Meta.Version != "8.4.11-11" {
		t.Errorf("meta = %+v", st.Meta)
	}
	if len(st.Sessions) != 4 {
		t.Fatalf("want 4 sessions, got %d", len(st.Sessions))
	}
	// The processlist carries the exact SQL each session was running.
	if st.Sessions[1].Query != "SELECT SLEEP(3600) AS holder_trx" || st.Sessions[1].GdbThread != 41 {
		t.Errorf("holder session = %+v", st.Sessions[1])
	}
	// The InnoDB lock-wait graph: 29863 (the UPDATE) is blocked by 29807 (the holder).
	if st.InnoDB == nil || len(st.InnoDB.Transactions) != 2 {
		t.Fatalf("innodb = %+v", st.InnoDB)
	}
	w := st.InnoDB.Transactions[0]
	if !w.Waiting || w.BlockedByTrx == nil || *w.BlockedByTrx != 29807 {
		t.Errorf("waiter = %+v", w)
	}
	if st.InnoDB.HistoryListLength == nil || *st.InnoDB.HistoryListLength != 90 {
		t.Errorf("history length = %v", st.InnoDB.HistoryListLength)
	}
	if st.Binlog == nil || st.Binlog.File != "./binlog.000002" || st.Binlog.Position == nil || *st.Binlog.Position != 1284718 {
		t.Errorf("binlog = %+v", st.Binlog)
	}
	if len(st.ErrorLog) != 2 || st.ErrorLog[0].Code != "MY-015017" {
		t.Errorf("error log = %+v", st.ErrorLog)
	}
	if st.Wsrep != nil {
		t.Errorf("a plain Percona Server core must carry no wsrep section, got %+v", st.Wsrep)
	}
}

const pxcServerStateLine = `<<CDA_SERVERSTATE>>{"meta":{"product":"pxc","version":"8.4.11-11","startTime":1791812000},` +
	`"sessions":[{"threadId":63,"command":"Query","db":"clab","query":"SELECT SLEEP(3600) AS pxc_long_select","state":"User sleep","gdbThread":30}],` +
	`"innodb":{"historyListLength":12,"transactions":[]},` +
	`"binlog":{"file":"./binlog.000004","position":55},` +
	`"errorLog":[{"ts":1791812500000000,"prio":0,"code":"MY-000000","subsys":"WSREP","message":"Member 2 synced"}],` +
	`"wsrep":{"ready":true,"clusterStatus":"Primary","clusterSize":3,"clusterName":"pxc-cluster-00",` +
	`"nodeName":"pxc03","nodeAddress":"pxc03.example.net","provider":"/usr/lib64/galera4/libgalera_smm.so",` +
	`"clusterAddress":"gcomm://pxc01.example.net,pxc02.example.net,pxc03.example.net","sstMethod":"xtrabackup-v2"}}`

func TestGDBParseServerState_PXC(t *testing.T) {
	st, err := gdbParseServerState("=library-loaded,...\n" + pxcServerStateLine + "\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.Wsrep == nil {
		t.Fatal("a PXC core must carry the wsrep section")
	}
	if st.Wsrep.ClusterStatus != "Primary" || st.Wsrep.ClusterSize == nil || *st.Wsrep.ClusterSize != 3 {
		t.Errorf("wsrep membership = %+v", st.Wsrep)
	}
	if st.Wsrep.NodeName != "pxc03" {
		t.Errorf("node name = %q", st.Wsrep.NodeName)
	}
	if st.Wsrep.Ready == nil || !*st.Wsrep.Ready {
		t.Errorf("ready = %v", st.Wsrep.Ready)
	}
}

func TestGDBParseServerState_NoLine(t *testing.T) {
	// No sentinel line at all — the symbols did not cover this build, say so rather than panic.
	if _, err := gdbParseServerState("Reading symbols...\n(gdb) quit\n"); err == nil {
		t.Fatal("want an error when the extractor produced no server-state line")
	}
}

func TestGDBSanitizeProduct(t *testing.T) {
	for in, want := range map[string]string{"pxc": "pxc", "ps": "ps", "": "ps", "pxc; rm -rf /": "ps", "PS": "ps"} {
		if got := gdbSanitizeProduct(in); got != want {
			t.Errorf("gdbSanitizeProduct(%q) = %q, want %q", in, got, want)
		}
	}
}

// The extractor is embedded; a build that dropped it would fail every live analysis with a blank
// script, so guard the //go:embed rather than discover it in production.
func TestGDBExtractScriptEmbedded(t *testing.T) {
	if !strings.Contains(gdbExtractScript, "PRODUCT_PLACEHOLDER") {
		t.Error("the embedded extractor lost its product placeholder")
	}
	if !strings.Contains(gdbExtractScript, gdbServerStateSentinel) {
		t.Error("the embedded extractor and the parser disagree on the sentinel")
	}
}
