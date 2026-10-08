package main

import (
	"strings"
	"testing"
	"time"
)

// livehealth_test.go — what the Live view's popups say is wrong, from the engines' own output.

func hasProblem(t *testing.T, r *liveRole, want string) {
	t.Helper()
	for _, p := range r.Problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Errorf("no problem containing %q in %q", want, r.Problems)
}

func TestMySQLReplicaBroken(t *testing.T) {
	out := `*************************** 1. row ***************************
ro: 1
*************************** 1. row ***************************
dumps: 0
*************************** 1. row ***************************
                  Source_Host: mysql-01.example.net
           Replica_IO_Running: Connecting
          Replica_SQL_Running: Yes
        Seconds_Behind_Source: NULL
                Last_IO_Error: error connecting to source 'repl@mysql-01.example.net:3306' - retry-time: 60 retries: 3
               Last_SQL_Error:
                 Channel_Name:
*************************** 1. row ***************************
                  Master_Host: mysql-01.example.net
             Slave_IO_Running: Connecting
`
	r := parseMySQLLive(out, 0)
	if r.Role != "replica" || r.State != "broken" || r.Access != "ro" {
		t.Fatalf("role %+v", r)
	}
	if len(r.Problems) != 1 {
		t.Fatalf("the same channel from SHOW SLAVE STATUS must not be reported twice: %q", r.Problems)
	}
	hasProblem(t, r, "receiver cannot connect to the source — error connecting to source")
}

func TestMySQLApplierError(t *testing.T) {
	out := `*************************** 1. row ***************************
ro: 0
*************************** 1. row ***************************
                  Source_Host: pxc-01.example.net
           Replica_IO_Running: Yes
          Replica_SQL_Running: No
        Seconds_Behind_Source: NULL
               Last_SQL_Error: Coordinator stopped because there were error(s) in the worker(s).
                 Channel_Name: xrepl_pxc-01
*************************** 2. row ***************************
                  Source_Host: pxc-11.example.net
           Replica_IO_Running: Yes
          Replica_SQL_Running: Yes
        Seconds_Behind_Source: 3
                 Channel_Name: xrepl_pxc-11
`
	r := parseMySQLLive(out, 0)
	if r.State != "1 of 2 channels running" {
		t.Errorf("state %q", r.State)
	}
	hasProblem(t, r, "pxc-01.example.net (channel xrepl_pxc-01): applier stopped — Coordinator stopped")
	if len(r.Problems) != 1 || r.LagSec == nil || *r.LagSec != 3 {
		t.Errorf("problems %q lag %v", r.Problems, r.LagSec)
	}
}

func TestMySQLHealthyReplicaHasNoProblems(t *testing.T) {
	out := `*************************** 1. row ***************************
ro: 1
*************************** 1. row ***************************
                  Source_Host: mysql-01.example.net
           Replica_IO_Running: Yes
          Replica_SQL_Running: Yes
        Seconds_Behind_Source: 0
                Last_IO_Error:
               Last_SQL_Error:
`
	r := parseMySQLLive(out, 0)
	if r.State != "replicating" || len(r.Problems) != 0 {
		t.Errorf("healthy replica: %+v", r)
	}
}

func TestGaleraLostMember(t *testing.T) {
	out := `*************************** 1. row ***************************
ro: 0
*************************** 1. row ***************************
wsrep: Synced
*************************** 1. row ***************************
wsrep_status: Primary
*************************** 1. row ***************************
wsrep_size: 2
`
	r := parseMySQLLive(out, 3)
	if r.Role != "member" || r.State != "Synced" {
		t.Fatalf("role %+v", r)
	}
	hasProblem(t, r, "sees 2 of 3 cluster members")
	if len(r.Problems) != 1 {
		t.Errorf("only the lost member is a problem: %q", r.Problems)
	}
	r = parseMySQLLive(strings.Replace(strings.Replace(out, "Primary", "non-Primary", 1), "Synced", "Initialized", 1), 3)
	hasProblem(t, r, "cluster component is non-Primary")
	hasProblem(t, r, "not synced with the cluster (Initialized)")
}

func TestGroupReplicationUnreachablePeer(t *testing.T) {
	out := `*************************** 1. row ***************************
ro: 0
*************************** 1. row ***************************
gr_role: PRIMARY
 gr_self: ONLINE
*************************** 1. row ***************************
gr_host: gr-02.example.net
gr_port: 3306
gr_state: ONLINE
*************************** 2. row ***************************
gr_host: gr-03.example.net
gr_port: 3306
gr_state: UNREACHABLE
`
	r := parseMySQLLive(out, 3)
	if r.Role != "primary" {
		t.Fatalf("role %+v", r)
	}
	hasProblem(t, r, "group member gr-03.example.net:3306 is UNREACHABLE")
	if len(r.Problems) != 1 {
		t.Errorf("problems %q", r.Problems)
	}
}

func TestStandalonePrimaryIsQuiet(t *testing.T) {
	r := parseMySQLLive("*************************** 1. row ***************************\nro: 0\n*************************** 1. row ***************************\ndumps: 2\n", 0)
	if r.Role != "primary" || *r.Replicas != 2 || len(r.Problems) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestPGReplicaNotStreaming(t *testing.T) {
	r := pgLiveRole(pgLive{Rec: true, RO: "on"})
	if r.Role != "replica" || r.Access != "ro" {
		t.Fatalf("%+v", r)
	}
	hasProblem(t, r, "not streaming from the primary")
	s := "streaming"
	if r := pgLiveRole(pgLive{Rec: true, Receiver: &s}); len(r.Problems) != 0 {
		t.Errorf("a streaming replica: %q", r.Problems)
	}
}

func TestPGPrimaryIdleSlot(t *testing.T) {
	r := pgLiveRole(pgLive{Senders: 1, IdleSlots: []string{"pg_03"}})
	if r.Role != "primary" {
		t.Fatalf("%+v", r)
	}
	hasProblem(t, r, "replication slot pg_03 has no replica connected")
	// Every replica gone: still a primary, with its slots saying so.
	r = pgLiveRole(pgLive{IdleSlots: []string{"pg_02", "pg_03"}})
	if r.Role != "primary" || len(r.Problems) != 2 {
		t.Errorf("%+v %q", r, r.Problems)
	}
	// A standby's slot copies are not lost replicas.
	s := "streaming"
	if r := pgLiveRole(pgLive{Rec: true, Receiver: &s, IdleSlots: []string{"pg_01"}}); len(r.Problems) != 0 {
		t.Errorf("standby slots: %q", r.Problems)
	}
}

// The case the Live view's member checks exist for: a node deleted from the canvas whose member is
// still in the replica set's config.
func TestMongoDeletedMemberStillInConfig(t *testing.T) {
	now := time.Now()
	members := []mongoMember{
		{Name: "psmdb-01.example.net:27017", Health: 1, StateStr: "PRIMARY", Self: true, Optime: now},
		{Name: "psmdb-02.example.net:27017", Health: 1, StateStr: "SECONDARY", Optime: now},
		{Name: "psmdb-03.example.net:27017", Health: 0, StateStr: "(not reachable/healthy)",
			Msg: "Error connecting to psmdb-03.example.net:27017 :: caused by :: Could not find address"},
	}
	problems, _ := mongoReplHealth(members)
	if len(problems) != 1 || !strings.Contains(problems[0], "cannot reach psmdb-03.example.net:27017 — Error connecting") {
		t.Fatalf("problems %q", problems)
	}

	// After rs.remove("psmdb-03…"), it is not a member and nothing is wrong.
	if problems, _ := mongoReplHealth(members[:2]); len(problems) != 0 {
		t.Errorf("removed member still reported: %q", problems)
	}
}

func TestMongoSecondaryView(t *testing.T) {
	now := time.Now()
	members := []mongoMember{
		{Name: "a:27017", Health: 1, StateStr: "PRIMARY", Optime: now},
		{Name: "b:27017", Health: 1, StateStr: "SECONDARY", Self: true, Optime: now.Add(-4 * time.Second)},
		{Name: "c:27017", Health: 1, StateStr: "RECOVERING"},
	}
	problems, lag := mongoReplHealth(members)
	if lag == nil || *lag < 3.9 || *lag > 4.1 {
		t.Errorf("lag %v", lag)
	}
	if len(problems) != 1 || problems[0] != "c:27017 is RECOVERING" {
		t.Errorf("problems %q", problems)
	}
	// Two members unreachable from the third: no primary either.
	problems, _ = mongoReplHealth([]mongoMember{
		{Name: "a:27017", StateStr: "SECONDARY", Self: true, Health: 1},
		{Name: "b:27017", Health: 0}, {Name: "c:27017", Health: 0},
	})
	if len(problems) != 3 || !strings.Contains(strings.Join(problems, "|"), "no primary") {
		t.Errorf("problems %q", problems)
	}
}

func TestValkeyReplicaLinkDown(t *testing.T) {
	out := "# Replication\r\nrole:slave\r\nmaster_host:valkey-01.example.net\r\nmaster_port:6379\r\nmaster_link_status:down\r\nmaster_link_down_since_seconds:42\r\nslave_read_only:1\r\n"
	r := parseValkeyLive(out)
	if r.Role != "replica" || r.Access != "ro" || r.State != "down" {
		t.Fatalf("%+v", r)
	}
	hasProblem(t, r, "link to primary valkey-01.example.net:6379 is down (for 42s)")
}

func TestValkeyClusterFailedNode(t *testing.T) {
	out := "# Replication\r\nrole:master\r\nconnected_slaves:0\r\n" +
		"#==cluster-info\ncluster_state:ok\r\ncluster_known_nodes:3\r\n" +
		"#==cluster-nodes\n" +
		"bea2 172.28.0.4:6379@16379 myself,master - 0 0 1 connected 0-5460\n" +
		"e85f 172.28.0.5:6379@16379 master - 0 1791428643467 2 connected 5461-10922\n" +
		"5395 172.28.0.6:6379@16379,valkey03.example.net master,fail - 1791428640000 1791428640000 3 disconnected 10923-16383\n"
	r := parseValkeyLive(out)
	if r.Role != "standalone" {
		t.Fatalf("%+v", r)
	}
	if len(r.Problems) != 1 || r.Problems[0] != "cannot reach valkey03.example.net (172.28.0.6:6379): the cluster has marked it failed" {
		t.Errorf("problems %q", r.Problems)
	}
	r = parseValkeyLive(strings.Replace(strings.Replace(out, "master,fail", "master,fail?", 1), "cluster_state:ok", "cluster_state:fail", 1))
	hasProblem(t, r, "cluster state is fail")
	hasProblem(t, r, "(suspected failing)")
}

func TestValkeyDown(t *testing.T) {
	r := parseValkeyLive("Could not connect to Valkey at 127.0.0.1:6379: Connection refused\n")
	if !r.Down || !strings.Contains(r.Err, "Connection refused") {
		t.Errorf("%+v", r)
	}
}

// The probe must not use client commands: the 9.x client refuses \G from a pipe ("Unknown command
// '\G'"), which failed the whole batch and every MySQL popup with it.
func TestMySQLProbeHasNoClientCommands(t *testing.T) {
	if strings.Contains(liveMySQLProbe, `\G`) {
		t.Error(`liveMySQLProbe uses \G; end statements with ";" and run the client with --vertical`)
	}
}

func TestUnreachableIsNotAQueryError(t *testing.T) {
	if !mysqlUnreachable("ERROR 2002 (HY000): Can't connect to local MySQL server through socket '/var/lib/mysql/mysql.sock' (2)") {
		t.Error("a missing socket is a server that is down")
	}
	if mysqlUnreachable("ERROR 1064 (42000) at line 1: You have an error in your SQL syntax") {
		t.Error("a syntax error is the probe's fault, not a down server")
	}
	if !pgUnreachable(`psql: error: connection to server on socket "/run/postgresql/.s.PGSQL.5432" failed: No such file or directory`) {
		t.Error("psql with no server is down")
	}
	if pgUnreachable(`ERROR:  column "sender_host" does not exist`) {
		t.Error("a query error is not a down server")
	}
}

// MariaDB 12 answers @@read_only with ON/OFF, not 1/0.
func TestMariaDBReadOnlyWord(t *testing.T) {
	r := parseMySQLLive("*************************** 1. row ***************************\nro: ON\n*************************** 1. row ***************************\n                  Master_Host: m-1\n             Slave_IO_Running: Yes\n            Slave_SQL_Running: Yes\n", 0)
	if r.Access != "ro" || r.Role != "replica" {
		t.Errorf("%+v", r)
	}
	if r := parseMySQLLive("*************************** 1. row ***************************\nro: NO_LOCK_NO_ADMIN\n", 0); r.Access != "ro" {
		t.Errorf("NO_LOCK_NO_ADMIN is read-only: %+v", r)
	}
	if r := parseMySQLLive("*************************** 1. row ***************************\nro: OFF\n", 0); r.Access != "rw" {
		t.Errorf("%+v", r)
	}
}

// MariaDB 10.5+ answers SHOW REPLICA STATUS and SHOW SLAVE STATUS alike, both with Master_* fields:
// one channel, one problem.
func TestMariaDBChannelNotCountedTwice(t *testing.T) {
	row := `*************************** 1. row ***************************
                Slave_IO_State:
                   Master_Host: mnogtid-2.example.net
              Slave_IO_Running: No
             Slave_SQL_Running: Yes
               Connection_name:
`
	out := "*************************** 1. row ***************************\nro: ON\n" + row + row
	r := parseMySQLLive(out, 0)
	if len(r.Problems) != 1 {
		t.Errorf("one channel reported %d times: %q", len(r.Problems), r.Problems)
	}
}
