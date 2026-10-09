package main

import (
	"strings"
	"testing"
)

const sampleDeadlock = "=====================================\n" +
	"------------------------\nLATEST DETECTED DEADLOCK\n------------------------\n" +
	"2026-10-09 08:01:02 140210000000000\n" +
	"*** (1) TRANSACTION:\nTRANSACTION 1859, ACTIVE 10 sec starting index read\nmysql tables in use 1, locked 1\n" +
	"LOCK WAIT 3 lock struct(s), heap size 1128, 2 row lock(s)\n" +
	"MySQL thread id 12, OS thread handle 140210, query id 101 localhost root updating\nUPDATE t SET v=2 WHERE id=2\n\n" +
	"*** (1) HOLDS THE LOCK(S):\nRECORD LOCKS space id 2 page no 4 n bits 72 index PRIMARY of table `test`.`t` trx id 1859 lock_mode X locks rec but not gap\n\n" +
	"*** (1) WAITING FOR THIS LOCK TO BE GRANTED:\nRECORD LOCKS space id 2 page no 4 n bits 72 index PRIMARY of table `test`.`t` trx id 1859 lock_mode X locks rec but not gap waiting\n\n" +
	"*** (2) TRANSACTION:\nTRANSACTION 1860, ACTIVE 8 sec starting index read\n" +
	"MySQL thread id 13, OS thread handle 140211, query id 102 localhost root updating\nUPDATE t SET v=1 WHERE id=1\n\n" +
	"*** (2) HOLDS THE LOCK(S):\nRECORD LOCKS space id 2 page no 4 n bits 72 index PRIMARY of table `test`.`t` trx id 1860 lock_mode X locks rec but not gap\n\n" +
	"*** (2) WAITING FOR THIS LOCK TO BE GRANTED:\nRECORD LOCKS space id 2 page no 4 n bits 72 index PRIMARY of table `test`.`t` trx id 1860 lock_mode X locks rec but not gap waiting\n\n" +
	"*** WE ROLL BACK TRANSACTION (2)\n------------\nTRANSACTIONS\n------------\nTrx id counter 1900\n"

func TestParseDeadlock(t *testing.T) {
	d := parseDeadlock(sampleDeadlock)
	if d == nil || d.At != "2026-10-09 08:01:02" || len(d.Txs) != 2 {
		t.Fatalf("%+v", d)
	}
	a, b := d.Txs[0], d.Txs[1]
	if a.Thread != "12" || a.Statement != "UPDATE t SET v=2 WHERE id=2" || a.Victim || !b.Victim {
		t.Fatalf("tx = %+v / %+v", a, b)
	}
	if !strings.Contains(a.Holds, "test.t") || !strings.Contains(a.Holds, "PRIMARY") || !strings.Contains(a.WaitsFor, "lock_mode X") {
		t.Fatalf("locks: holds %q waits %q", a.Holds, a.WaitsFor)
	}
	if strings.Contains(d.Raw, "Trx id counter") {
		t.Error("the raw section runs into TRANSACTIONS")
	}
	if a.User != "root@localhost" {
		t.Errorf("user = %q", a.User)
	}
	for in, want := range map[string]string{
		"localhost 127.0.0.1 app updating": "app@localhost",
		"10.0.0.5 app statistics":          "app@10.0.0.5",
		"app.example.com 10.0.0.5 app":     "app@app.example.com",
	} {
		if got := deadlockUser(strings.Fields(in)); got != want {
			t.Errorf("deadlockUser(%q) = %q, want %q", in, got, want)
		}
	}
	if parseDeadlock("no deadlock here") != nil {
		t.Error("no section is no deadlock")
	}
}

// The classic pile-up: a long SELECT holds SHARED_READ, an ALTER waits for EXCLUSIVE, and every
// new SELECT queues behind the ALTER — the SELECTs' blocker is the ALTER, the ALTER's the SELECT.
func TestMDLQueue(t *testing.T) {
	rows := []struct {
		T                 jnum
		Obj, Type, Status string
	}{
		{"10", "app.orders", "SHARED_READ", "GRANTED"},
		{"20", "app.orders", "EXCLUSIVE", "PENDING"},
		{"30", "app.orders", "SHARED_READ", "PENDING"},
		{"31", "app.orders", "SHARED_WRITE", "PENDING"},
	}
	ws := mdlWaits(rows, map[string]*actSession{})
	got := map[string]string{}
	for _, w := range ws {
		got[w.Waiter] += w.Blocker + ","
	}
	if got["20"] != "10," || got["30"] != "20," || got["31"] != "20," {
		t.Fatalf("waits = %v", got)
	}
}

func TestIsDDL(t *testing.T) {
	for _, s := range []string{"ALTER TABLE t ADD c int", "/* gh-ost */ alter table `x`", "create unique index i on t(a)", "OPTIMIZE TABLE t", "VACUUM FULL t"} {
		if !isDDL(s) {
			t.Errorf("%q is DDL", s)
		}
	}
	for _, s := range []string{"SELECT * FROM t WHERE altered = 1", "UPDATE t SET a=1", ""} {
		if isDDL(s) {
			t.Errorf("%q is not DDL", s)
		}
	}
}

func TestPgBouncerActivity(t *testing.T) {
	out := "##POOLS##\n" +
		"database\tuser\tcl_active\tcl_waiting\tcl_active_cancel_req\tcl_waiting_cancel_req\tsv_active\tsv_active_cancel\tsv_being_canceled\tsv_idle\tsv_used\tsv_tested\tsv_login\tmaxwait\tmaxwait_us\tpool_mode\tload_balance_hosts\n" +
		"pgbouncer\tpgbouncer\t1\t0\t0\t0\t0\t0\t0\t0\t0\t0\t0\t0\t0\tstatement\t\n" +
		"app\tpostgres\t5\t3\t0\t0\t5\t0\t0\t0\t0\t0\t0\t4\t120000\ttransaction\t\n" +
		"##CLIENTS##\n" +
		"type\tuser\tdatabase\treplication\tstate\taddr\tport\tlocal_addr\tlocal_port\tconnect_time\trequest_time\twait\twait_us\tclose_needed\tptr\tlink\tremote_pid\ttls\tapplication_name\tprepared_statements\tid\n" +
		"C\tpostgres\tapp\tnone\twaiting\t10.0.0.5\t51234\t10.0.0.9\t6432\t2026-10-09 08:00:00 UTC\t2026-10-09 08:00:01 UTC\t4\t120000\t0\t0x1\t\t0\t\tcarsim\t0\t17\n"
	s := parsePgBouncerActivity(out)
	if len(s.Proxy.Pools) != 1 || s.Proxy.Pools[0].Waiting != 3 || s.Proxy.Pools[0].MaxWait != 4.12 || s.Proxy.Pools[0].Status != "transaction" {
		t.Fatalf("pools = %+v", s.Proxy.Pools)
	}
	if len(s.Sessions) != 1 || s.Sessions[0].ID != "17" || s.Sessions[0].App != "carsim" || s.Sessions[0].Host != "10.0.0.5:51234" {
		t.Fatalf("sessions = %+v", s.Sessions[0])
	}
	if len(s.Waits) != 1 || s.Waits[0].Kind != "pool" {
		t.Fatalf("waits = %+v", s.Waits)
	}
}

func TestTxnFrom(t *testing.T) {
	x := txnFrom("700", "2", "45")
	if x == nil || x.OldestSec != 700 || x.LockWaiters != 2 || x.LockWaitMaxSec != 45 {
		t.Fatalf("%+v", x)
	}
	if txnFrom("", "", "") != nil {
		t.Error("nothing given is nil")
	}
}

func TestLockedKey(t *testing.T) {
	for in, want := range map[string]string{
		" 0: len 4; hex 80000001; asc     ;;":             "key 1",
		" 0: len 8; hex 8000000000000016; asc         ;;": "key 22",
		" 0: len 4; hex 7ffffffe; asc     ;;":             "key -2",
		" 0: len 5; hex 616c696365; asc alice;;":          "key 'alice'",
		"Record lock, heap no 1 PHYSICAL RECORD: n_fields 1; compact format; info bits 0\n 0: len 8; hex 73757072656d756d; asc supremum;;": "the end of the index (a gap lock)",
	} {
		if got := lockedKey(in); got != want {
			t.Errorf("lockedKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePGDeadlocks(t *testing.T) {
	log := "2026-10-09 00:28:48.001 UTC [2711] LOG:  checkpoint complete\n" +
		"2026-10-09 00:28:49.019 UTC [6987] ERROR:  deadlock detected\n" +
		"2026-10-09 00:28:49.019 UTC [6987] DETAIL:  Process 6987 waits for ShareLock on transaction 773; blocked by process 6989.\n" +
		"\tProcess 6989 waits for ShareLock on transaction 772; blocked by process 6987.\n" +
		"\tProcess 6987: update pt set v=v+1 where id=3\n" +
		"\tProcess 6989: update pt set v=v+1\n" +
		"\t  where id=2\n" +
		"2026-10-09 00:28:49.019 UTC [6987] HINT:  See server log for query details.\n" +
		"2026-10-09 00:28:49.019 UTC [6987] CONTEXT:  while updating tuple (0,4) in relation \"pt\"\n" +
		"2026-10-09 00:28:49.019 UTC [6987] STATEMENT:  update pt set v=v+1 where id=3\n" +
		"2026-10-09 00:28:53.144 UTC [2711] LOG:  checkpoint starting: time\n"
	ds := parsePGDeadlocks(log)
	if len(ds) != 1 {
		t.Fatalf("%d deadlocks", len(ds))
	}
	d := ds[0]
	if d.At != "2026-10-09 00:28:49" || len(d.Txs) != 2 || d.Engine != "postgres" {
		t.Fatalf("%+v", d)
	}
	a, b := d.Txs[0], d.Txs[1]
	if a.Thread != "6987" || !a.Victim || b.Victim || a.Statement != "update pt set v=v+1 where id=3" {
		t.Fatalf("a = %+v", a)
	}
	if b.Statement != "update pt set v=v+1\n  where id=2" || b.Holds != "the rows its transaction 773 changed" {
		t.Fatalf("b = %+v", b)
	}
	if !strings.Contains(a.WaitsFor, "held by process 6989") || !strings.Contains(a.WaitsFor, "a row in pt") {
		t.Fatalf("waits = %q", a.WaitsFor)
	}
	if strings.Contains(d.Raw, "checkpoint") {
		t.Errorf("raw runs past the report: %q", d.Raw)
	}
}
