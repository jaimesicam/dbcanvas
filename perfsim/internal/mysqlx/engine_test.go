package mysqlx

import "testing"

// The replica statements must match what each server accepts: REPLICA and SOURCE
// from MySQL 8.0.23, SLAVE and MASTER before that and on MariaDB.
func TestWords(t *testing.T) {
	for _, c := range []struct {
		flavor, version, stop, delay, workers, applier string
	}{
		{"percona", "8.4.5", "STOP REPLICA SQL_THREAD", "CHANGE REPLICATION SOURCE TO SOURCE_DELAY = %d", "replica_parallel_workers", "wsrep_applier_threads"},
		{"mysql", "8.0.22", "STOP SLAVE SQL_THREAD", "CHANGE MASTER TO MASTER_DELAY = %d", "slave_parallel_workers", "wsrep_slave_threads"},
		{"percona", "5.7.44", "STOP SLAVE SQL_THREAD", "CHANGE MASTER TO MASTER_DELAY = %d", "slave_parallel_workers", "wsrep_slave_threads"},
		{"mariadb", "11.4.2", "STOP SLAVE SQL_THREAD", "CHANGE MASTER TO MASTER_DELAY = %d", "slave_parallel_threads", "wsrep_slave_threads"},
	} {
		w := words(c.flavor, c.version)
		if w["stop"] != c.stop || w["delay"] != c.delay || w["workers"] != c.workers || w["applier"] != c.applier {
			t.Errorf("%s %s: %v", c.flavor, c.version, w)
		}
	}
	if words("mariadb", "11.4.2")["single"] != "0" || words("mysql", "8.4.0")["single"] != "1" {
		t.Error("single-threaded value: MariaDB 0, MySQL 1")
	}
}

// Every dial is listed on every topology; a standalone server has no lag dial
// available, and the ones a topology cannot do say why. The load dials act on
// the writer alone, so every topology has them.
func TestDialsAvailability(t *testing.T) {
	avail := func(topo string) map[string]bool {
		e := &Engine{topology: topo, vars: words("percona", "8.4.5")}
		out := map[string]bool{}
		for _, d := range e.Dials() {
			out[d.ID] = d.Available
			if !d.Available && d.Why == "" {
				t.Errorf("%s: %s unavailable with no reason", topo, d.ID)
			}
		}
		return out
	}
	load := map[string]bool{}
	for _, d := range (&Engine{}).loadDials() {
		load[d.ID] = true
	}
	for id, ok := range avail("standalone") {
		if ok != load[id] {
			t.Errorf("standalone: %s available", id)
		}
	}
	for topo, want := range map[string]map[string]bool{
		"async":  {"bigtxn": true, "alter": true, "ftwrl": true, "stopapplier": true, "nopk": true, "singlethread": true, "delay": true, "mdl": true},
		"gr":     {"bigtxn": true, "alter": true, "ftwrl": true, "stopapplier": false, "nopk": false, "singlethread": false, "delay": false, "mdl": true},
		"galera": {"bigtxn": true, "alter": true, "ftwrl": true, "stopapplier": false, "nopk": false, "singlethread": true, "delay": false, "mdl": false},
	} {
		got := avail(topo)
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s: %s available %v, want %v", topo, id, got[id], w)
			}
		}
	}
}
